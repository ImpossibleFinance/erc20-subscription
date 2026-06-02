// Package scheduler runs the periodic "find due subs, charge them" loop.
//
// Each tick:
//  1. resolve any in-flight tx from the previous tick (so we don't
//     double-pull if a previous submission's receipt was lost),
//  2. pull due subs synchronously: submit pull(), wait for receipt, update
//     store + webhook on the result.
//
// Single-loop, sequential — throughput is bounded by chain pacing anyway
// (one operator nonce at a time), so adding parallelism mostly creates
// nonce contention without buying anything.
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

	"github.com/impossiblefinance/erc20-subscription/backend/internal/chain"
	"github.com/impossiblefinance/erc20-subscription/backend/internal/dunning"
	"github.com/impossiblefinance/erc20-subscription/backend/internal/store"
	"github.com/impossiblefinance/erc20-subscription/backend/internal/webhooks"
)

type Scheduler struct {
	chain              *chain.Client
	store              store.Store
	policy             dunning.Policy
	webhook            *webhooks.Sender
	interval           time.Duration
	batch              int
	now                func() time.Time
	tokenAddr          common.Address // ERC-20 we're pulling; needed for allowance checks
	allowanceLowMonths int            // 0 disables the post-pull allowance warning
}

type Config struct {
	Policy             dunning.Policy
	Interval           time.Duration
	TokenAddr          common.Address
	AllowanceLowMonths int
}

func New(c *chain.Client, s store.Store, w *webhooks.Sender, cfg Config) *Scheduler {
	return &Scheduler{
		chain:              c,
		store:              s,
		policy:             cfg.Policy,
		webhook:            w,
		interval:           cfg.Interval,
		batch:              25,
		now:                func() time.Time { return time.Now().UTC() },
		tokenAddr:          cfg.TokenAddr,
		allowanceLowMonths: cfg.AllowanceLowMonths,
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

	price, ok := new(big.Int).SetString(plan.PriceAtomic, 10)
	if !ok {
		return s.markCancelled(ctx, sub, "plan_price_invalid")
	}

	hash, err := s.chain.Pull(ctx, common.HexToAddress(sub.User), price)
	if err != nil {
		// Submission failure — likely insufficient allowance/balance
		// surfaced as a gas-estimate revert, or a transient RPC issue.
		// Either way, treat as a payment failure and let dunning decide.
		return s.handleFailure(ctx, sub, plan, err)
	}

	// Record the in-flight hash BEFORE waiting for the receipt. If the wait
	// is interrupted (process restart, ctx cancel), the next tick will
	// resolve it via TransactionReceipt before submitting another pull.
	sub.InFlightTx = hash.Hex()
	if err := s.store.UpsertSubscription(ctx, sub); err != nil {
		return fmt.Errorf("persist in-flight: %w", err)
	}

	receipt, err := s.chain.WaitReceipt(ctx, hash)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// Don't clear InFlightTx — next tick will resolve.
			return nil
		}
		// Tx reverted on-chain. WaitReceipt returns (receipt, err) here.
		_ = receipt
		return s.handleFailure(ctx, sub, plan, err)
	}

	return s.markCharged(ctx, sub, plan, hash, receipt.BlockNumber.Uint64())
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
	sub.DunningAttempts++
	sub.LastError = truncate(cause.Error(), 200)
	sub.InFlightTx = ""

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
			"user":           sub.User,
			"plan_id":        sub.PlanID,
			"amount_atomic":  plan.PriceAtomic,
			"attempts":       sub.DunningAttempts,
			"next_attempt":   sub.NextAttemptAt,
			"reason":         sub.LastError,
		})
	}
	return nil
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
