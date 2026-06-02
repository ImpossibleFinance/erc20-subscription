// Package scheduler runs the periodic "find due subs, charge them" loop.
//
// Each tick:
//  1. resolve any in-flight tx from the previous tick (so we don't
//     double-pull if a previous submission's receipt was lost),
//  2. pull due subs SEQUENTIALLY: submit pull(), wait for receipt, update
//     store + webhook on the result, then the next sub.
//
// Sequential is mandatory, not stylistic — the operator EOA has a single
// nonce sequence. Two concurrent pull() submissions would either share a
// nonce (one fails with "already known") or step on each other in the
// mempool. The chain itself is the throughput bound, so single-threaded
// is the right model. The only place we wait on the network in parallel
// is webhook delivery, which doesn't touch the chain.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/impossiblefinance/erc20-subscription/backend/internal/chain"
	"github.com/impossiblefinance/erc20-subscription/backend/internal/dunning"
	"github.com/impossiblefinance/erc20-subscription/backend/internal/store"
	"github.com/impossiblefinance/erc20-subscription/backend/internal/webhooks"
)

type Scheduler struct {
	chain                   *chain.Client
	store                   store.Store
	policy                  dunning.Policy
	webhook                 *webhooks.Sender
	interval                time.Duration
	batch                   int
	now                     func() time.Time
	tokenAddr               common.Address
	allowanceLowMonths      int
	operatorGasBufferMonths int           // warn when balance covers fewer than N months of upcoming pulls
	operatorGasWarnInterval time.Duration // dedup window for the gas warning
	lastGasWarnAt           time.Time
}

type Config struct {
	Policy                  dunning.Policy
	Interval                time.Duration
	TokenAddr               common.Address
	AllowanceLowMonths      int
	OperatorGasBufferMonths int
	OperatorGasWarnInterval time.Duration
}

func New(c *chain.Client, s store.Store, w *webhooks.Sender, cfg Config) *Scheduler {
	return &Scheduler{
		chain:                   c,
		store:                   s,
		policy:                  cfg.Policy,
		webhook:                 w,
		interval:                cfg.Interval,
		batch:                   25,
		now:                     func() time.Time { return time.Now().UTC() },
		tokenAddr:               cfg.TokenAddr,
		allowanceLowMonths:      cfg.AllowanceLowMonths,
		operatorGasBufferMonths: cfg.OperatorGasBufferMonths,
		operatorGasWarnInterval: cfg.OperatorGasWarnInterval,
	}
}

func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Tick(ctx); err != nil {
				log.Printf("scheduler: tick: %v", err)
			}
		}
	}
}

func (s *Scheduler) Tick(ctx context.Context) error {
	// Check operator gas first so a low-balance warning fires even on ticks
	// where nothing is due.
	s.maybeWarnOperatorGas(ctx)

	due, err := s.store.DueBefore(ctx, s.now(), s.batch)
	if err != nil {
		return err
	}
	for _, sub := range due {
		if err := s.processOne(ctx, sub); err != nil {
			log.Printf("scheduler: user=%s: %v", sub.User, err)
		}
	}
	return nil
}

// Gas-per-pull estimate. The Subscriptions.pull tx does one storage read
// (operator check) and one ERC-20 transferFrom (~50-65k gas). 100k is a
// conservative ceiling that survives most network conditions.
const estimatedGasPerPull = 100_000

// maybeWarnOperatorGas checks whether the operator's ETH balance covers at
// least `operatorGasBufferMonths` of upcoming pulls at the current gas price,
// and fires an `operator.gas_low` webhook if not. The threshold is computed
// dynamically — there's no hardcoded wei value to keep in sync with usage.
//
// We don't HALT pulls when gas is low. If the threshold computation or RPC
// is wrong, halting would cascade into spurious payment_failed events for
// users. Better to warn loudly and let the admin top up.
//
// Rate-limited via operatorGasWarnInterval (default 6h) so a persistent
// low-balance condition doesn't spam ops.
func (s *Scheduler) maybeWarnOperatorGas(ctx context.Context) {
	if s.operatorGasBufferMonths == 0 || s.webhook == nil {
		return
	}
	if !s.lastGasWarnAt.IsZero() && s.now().Sub(s.lastGasWarnAt) < s.operatorGasWarnInterval {
		return
	}
	threshold, pullsCount, err := s.computeGasThreshold(ctx)
	if err != nil || threshold == nil {
		// Couldn't compute — log but don't false-alarm.
		if err != nil {
			log.Printf("scheduler: gas threshold: %v", err)
		}
		return
	}
	bal, err := s.chain.Balance(ctx, s.chain.OperatorAddress())
	if err != nil {
		log.Printf("scheduler: operator balance: %v", err)
		return
	}
	if bal.Cmp(threshold) >= 0 {
		return
	}
	_ = s.webhook.Send(ctx, webhooks.EventOperatorGasLow, map[string]any{
		"operator":         s.chain.OperatorAddress().Hex(),
		"balance_wei":      bal.String(),
		"threshold_wei":    threshold.String(),
		"buffer_months":    s.operatorGasBufferMonths,
		"pulls_in_horizon": pullsCount,
	})
	s.lastGasWarnAt = s.now()
}

// computeGasThreshold returns the wei amount needed to cover all pulls due
// in the next `operatorGasBufferMonths` months, at current gas price, with a
// 2× safety multiplier for gas-market spikes. Also returns the number of
// pulls counted, for the webhook payload.
func (s *Scheduler) computeGasThreshold(ctx context.Context) (*big.Int, int, error) {
	horizon := s.now().AddDate(0, s.operatorGasBufferMonths, 0)
	// DueBefore returns active+past_due subs whose NextAttemptAt ≤ horizon
	// OR which have a pending one-time charge. That's our upper bound on
	// pull count over the buffer window — assumes ~1 pull per sub, which is
	// accurate for monthly plans (the common case). Short-period plans
	// (daily/weekly) undercount; we lean on the 2× safety multiplier and
	// document the assumption in the README.
	subs, err := s.store.DueBefore(ctx, horizon, 100_000)
	if err != nil {
		return nil, 0, err
	}
	pulls := int64(len(subs))
	if pulls == 0 {
		// Even with zero subs, keep a small minimum so the operator has gas
		// to cover the first signup that arrives. 10 pulls' worth.
		pulls = 10
	}

	perGas, err := s.chain.EstimatedGasPricePerUnit(ctx)
	if err != nil {
		return nil, 0, err
	}
	perPull := new(big.Int).Mul(big.NewInt(estimatedGasPerPull), perGas)
	threshold := new(big.Int).Mul(perPull, big.NewInt(pulls))
	threshold.Mul(threshold, big.NewInt(2)) // 2× safety for gas spikes
	return threshold, int(pulls), nil
}

// processOne resolves an in-flight tx (if any) and then attempts a fresh pull
// if the sub is still due.
func (s *Scheduler) processOne(ctx context.Context, sub *store.Subscription) error {
	if sub.InFlightTx != "" {
		if err := s.resolveInFlight(ctx, sub); err != nil {
			return fmt.Errorf("resolve in-flight: %w", err)
		}
		// Re-fetch to pick up updates; processing ends here for this tick
		// regardless of outcome — next tick will either retry (if due) or
		// skip (if NextAttemptAt was advanced).
		return nil
	}

	plan, err := s.store.GetPlan(ctx, sub.PlanID)
	if err != nil {
		return fmt.Errorf("get plan: %w", err)
	}
	if plan == nil || !plan.Active {
		// Plan disappeared or was deactivated mid-cycle; mark the sub
		// cancelled. Future charges won't resume even if the plan is
		// re-activated unless the integrator creates a new sub.
		return s.markCancelled(ctx, sub, "plan_missing_or_inactive")
	}

	// If the sub has a pending one-time charge (e.g. prorated upgrade diff),
	// settle that first. We don't advance NextAttemptAt here — the regular
	// cycle continues on its existing anchor.
	if sub.PendingChargeAtomic != "" {
		return s.pullPending(ctx, sub, plan)
	}

	price, ok := new(big.Int).SetString(plan.PriceAtomic, 10)
	if !ok {
		return s.markCancelled(ctx, sub, "plan_price_invalid")
	}

	receipt, err := s.submitAndWait(ctx, sub, price)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil // resume next tick via InFlightTx
		}
		if errors.Is(err, chain.ErrTxStuck) {
			s.emitTxStuck(ctx, sub, err)
			return nil // sub stays as-is; admin must intervene
		}
		return s.handleFailure(ctx, sub, plan, err)
	}
	return s.markCharged(ctx, sub, plan, common.HexToHash(sub.InFlightTx), receipt.BlockNumber.Uint64())
}

// submitAndWait runs the bump-aware submitter and persists InFlightTx after
// every broadcast so a restart can resolve in-flight state.
func (s *Scheduler) submitAndWait(ctx context.Context, sub *store.Subscription, amount *big.Int) (*types.Receipt, error) {
	return s.chain.SubmitPull(ctx, common.HexToAddress(sub.User), amount, func(h common.Hash) error {
		sub.InFlightTx = h.Hex()
		return s.store.UpsertSubscription(ctx, sub)
	})
}

// emitTxStuck logs and emits the operator.tx_stuck webhook so ops know a
// pull is pinned in the mempool. The operator's nonce is held until this tx
// resolves; no further pulls can proceed.
func (s *Scheduler) emitTxStuck(ctx context.Context, sub *store.Subscription, cause error) {
	log.Printf("scheduler: tx stuck for user=%s: %v", sub.User, cause)
	if s.webhook == nil {
		return
	}
	_ = s.webhook.Send(ctx, webhooks.EventOperatorTxStuck, map[string]any{
		"operator":     s.chain.OperatorAddress().Hex(),
		"user":         sub.User,
		"plan_id":      sub.PlanID,
		"in_flight_tx": sub.InFlightTx,
		"reason":       cause.Error(),
	})
}

// pullPending pulls the one-time charge stashed on the sub (a proration diff
// from a plan change). On success we clear PendingChargeAtomic — the regular
// cycle continues unchanged. On failure we dunning the sub like any other
// payment problem.
func (s *Scheduler) pullPending(ctx context.Context, sub *store.Subscription, plan *store.Plan) error {
	amount, ok := new(big.Int).SetString(sub.PendingChargeAtomic, 10)
	if !ok || amount.Sign() <= 0 {
		// Invalid pending amount; clear it and continue normally next tick.
		sub.PendingChargeAtomic = ""
		return s.store.UpsertSubscription(ctx, sub)
	}
	receipt, err := s.submitAndWait(ctx, sub, amount)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil
		}
		if errors.Is(err, chain.ErrTxStuck) {
			s.emitTxStuck(ctx, sub, err)
			return nil
		}
		return s.handleFailure(ctx, sub, plan, err)
	}

	sub.PendingChargeAtomic = ""
	sub.LastChargedTx = sub.InFlightTx
	sub.LastChargedBlock = receipt.BlockNumber.Uint64()
	sub.LastChargedAt = s.now()
	sub.InFlightTx = ""
	if err := s.store.UpsertSubscription(ctx, sub); err != nil {
		return err
	}
	if s.webhook != nil {
		_ = s.webhook.Send(ctx, webhooks.EventProratedCharge, map[string]any{
			"user":          sub.User,
			"plan_id":       sub.PlanID,
			"amount_atomic": amount.String(),
			"tx_hash":       sub.LastChargedTx,
			"block":         sub.LastChargedBlock,
		})
	}
	s.maybeWarnAllowanceLow(ctx, sub, plan)
	return nil
}

// resolveInFlight polls a previously-submitted tx hash. The store is updated
// based on the outcome; InFlightTx is cleared either way.
func (s *Scheduler) resolveInFlight(ctx context.Context, sub *store.Subscription) error {
	plan, _ := s.store.GetPlan(ctx, sub.PlanID)
	if plan == nil {
		sub.InFlightTx = ""
		return s.store.UpsertSubscription(ctx, sub)
	}

	hash := common.HexToHash(sub.InFlightTx)
	receipt, err := s.chain.WaitReceipt(ctx, hash)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		// On revert: dunning. On ambiguity (no receipt yet), leave for next tick.
		if receipt != nil {
			return s.handleFailure(ctx, sub, plan, err)
		}
		// Receipt not found AND not a ctx error → keep in-flight for now;
		// tx might still mine. Don't double-spend by retrying.
		return nil
	}
	if receipt == nil {
		return nil
	}
	return s.markCharged(ctx, sub, plan, hash, receipt.BlockNumber.Uint64())
}

func (s *Scheduler) markCharged(ctx context.Context, sub *store.Subscription, plan *store.Plan, hash common.Hash, block uint64) error {
	now := s.now()
	prevAttempt := sub.NextAttemptAt
	if prevAttempt.IsZero() {
		prevAttempt = now
	}
	// Advance by the plan's period from the SCHEDULED attempt, not from now.
	// A late pull doesn't delay future ones — users pay the same total per
	// year. plan.AddPeriod handles calendar-month/year arithmetic (clamping
	// Jan 31 → Feb 28, etc.) instead of treating months as 30-day blocks.
	sub.NextAttemptAt = plan.AddPeriod(prevAttempt)
	sub.Status = store.StatusActive
	sub.DunningAttempts = 0
	sub.LastError = ""
	sub.LastChargedTx = hash.Hex()
	sub.LastChargedBlock = block
	sub.LastChargedAt = now
	sub.InFlightTx = ""
	if err := s.store.UpsertSubscription(ctx, sub); err != nil {
		return err
	}
	if s.webhook != nil {
		_ = s.webhook.Send(ctx, webhooks.EventCharged, map[string]any{
			"user":          sub.User,
			"plan_id":       sub.PlanID,
			"amount_atomic": plan.PriceAtomic,
			"period_count":  plan.PeriodCount,
			"period_unit":   plan.PeriodUnit,
			"tx_hash":       sub.LastChargedTx,
			"block":         sub.LastChargedBlock,
			"next_attempt":  sub.NextAttemptAt,
		})
	}
	s.maybeWarnAllowanceLow(ctx, sub, plan)
	return nil
}

// maybeWarnAllowanceLow reads the user's remaining USDC allowance and emits a
// warning if it covers fewer than `allowanceLowMonths` additional pulls. Best-
// effort: any failure here is logged and swallowed — the pull itself already
// succeeded.
func (s *Scheduler) maybeWarnAllowanceLow(ctx context.Context, sub *store.Subscription, plan *store.Plan) {
	if s.allowanceLowMonths == 0 || s.webhook == nil {
		return
	}
	if (s.tokenAddr == common.Address{}) {
		return
	}
	owner := common.HexToAddress(sub.User)
	allowance, err := s.chain.Allowance(ctx, s.tokenAddr, owner, s.chain.Contract())
	if err != nil {
		log.Printf("scheduler: allowance(%s): %v", sub.User, err)
		return
	}
	price, ok := new(big.Int).SetString(plan.PriceAtomic, 10)
	if !ok || price.Sign() <= 0 {
		return
	}
	threshold := new(big.Int).Mul(price, big.NewInt(int64(s.allowanceLowMonths)))
	if allowance.Cmp(threshold) >= 0 {
		return
	}
	remaining := new(big.Int).Quo(allowance, price)
	_ = s.webhook.Send(ctx, webhooks.EventAllowanceLow, map[string]any{
		"user":              sub.User,
		"plan_id":           sub.PlanID,
		"allowance_atomic":  allowance.String(),
		"price_atomic":      plan.PriceAtomic,
		"remaining_periods": remaining.Int64(),
		"threshold_periods": s.allowanceLowMonths,
	})
}

func (s *Scheduler) handleFailure(ctx context.Context, sub *store.Subscription, plan *store.Plan, cause error) error {
	now := s.now()
	sub.InFlightTx = ""

	// Operator-side errors (out of gas, RPC down, contract paused) are not
	// the user's fault — don't dunning them. Back off a short interval so
	// the next tick retries naturally once the admin tops up / fixes the
	// problem. The admin is notified via the `operator.gas_low` webhook
	// (forced now so they hear about it even mid-dedup window).
	if isOperatorSideError(cause) {
		log.Printf("scheduler: operator-side error for %s: %v", sub.User, cause)
		s.maybeWarnOperatorGas(ctx)
		sub.LastError = truncate(cause.Error(), 200)
		// Keep Status active and DunningAttempts unchanged — this isn't
		// the user's failure to dunning against. Retry in 5 minutes.
		sub.NextAttemptAt = now.Add(5 * time.Minute)
		return s.store.UpsertSubscription(ctx, sub)
	}

	// User-side error — dunning.
	sub.DunningAttempts++
	sub.LastError = truncate(cause.Error(), 200)

	next, ok := s.policy.NextRetry(sub.DunningAttempts, now)
	if !ok {
		return s.markCancelled(ctx, sub, sub.LastError)
	}

	sub.Status = store.StatusPastDue
	sub.NextAttemptAt = next
	if err := s.store.UpsertSubscription(ctx, sub); err != nil {
		return err
	}
	if s.webhook != nil {
		_ = s.webhook.Send(ctx, webhooks.EventPaymentFailed, map[string]any{
			"user":          sub.User,
			"plan_id":       sub.PlanID,
			"amount_atomic": plan.PriceAtomic,
			"attempts":      sub.DunningAttempts,
			"next_attempt":  sub.NextAttemptAt,
			"reason":        sub.LastError,
		})
	}
	return nil
}

// isOperatorSideError classifies a pull failure as "we / our infra screwed
// up" vs "the user can't pay." Operator-side errors should never advance the
// user's dunning counter or cancel their subscription.
//
// We match by error message because go-ethereum surfaces RPC errors as
// strings; no typed sentinels. The patterns below cover:
//   - operator EOA out of ETH (estimate + send)
//   - gas market spikes that put our maxFee under the base fee
//   - nonce mismatch (mid-flight rotation)
//   - RPC infra hiccups
//   - the contract being halted by the owner (operator==0)
func isOperatorSideError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	patterns := []string{
		"insufficient funds for gas",
		"insufficient funds for transfer",
		"max fee per gas less than block base fee",
		"transaction underpriced",
		"replacement transaction underpriced",
		"nonce too low",
		"nonce too high",
		"gas required exceeds allowance",
		"context deadline exceeded",
		"connection refused",
		"i/o timeout",
		"no such host",
		// Subscriptions contract: operator was rotated to 0 or the owner
		// otherwise revoked us. Same effect as halted.
		"notoperator",
	}
	for _, p := range patterns {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

func (s *Scheduler) markCancelled(ctx context.Context, sub *store.Subscription, reason string) error {
	sub.Status = store.StatusCancelled
	sub.NextAttemptAt = s.now().AddDate(100, 0, 0) // remove from due ZSET
	sub.LastError = truncate(reason, 200)
	sub.InFlightTx = ""
	if err := s.store.UpsertSubscription(ctx, sub); err != nil {
		return err
	}
	if s.webhook != nil {
		_ = s.webhook.Send(ctx, webhooks.EventCancelled, map[string]any{
			"user":    sub.User,
			"plan_id": sub.PlanID,
			"reason":  sub.LastError,
		})
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// NormalizeAddr lowercases and 0x-pads a hex address. Used at API edges to
// keep all store keys consistent.
func NormalizeAddr(h string) string { return strings.ToLower(common.HexToAddress(h).Hex()) }
