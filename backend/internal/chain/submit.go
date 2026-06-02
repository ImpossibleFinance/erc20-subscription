package chain

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// SubmitAndWait submits a contract call and waits for the receipt. If the tx
// doesn't mine within `submitPerAttempt`, the client diagnoses WHY before
// deciding to bump:
//
//   - If `maxFeePerGas < current baseFee` → bump (we underbid; needed).
//   - If `tip < current suggested tip` → bump (mempool wants more; needed).
//   - Otherwise → just wait. The fee is fine; the network is slow or the L2
//     sequencer is congested. Bumping here would only waste gas.
//
// Caps at `submitMaxBumps` active bumps; total wall time is bounded by
// `submitTotalBudget`. Beyond either, returns ErrTxStuck — the caller surfaces
// this to ops (via operator.tx_stuck webhook) so the admin can intervene.
//
// `onSubmit` is called after every successful SendTransaction so the caller
// can persist the latest hash; this is what lets the scheduler resolve an
// in-flight tx across a server restart.
func (c *Client) SubmitAndWait(ctx context.Context, method string, args []interface{}, onSubmit func(common.Hash) error) (*types.Receipt, error) {
	data, err := SubscriptionsABI.Pack(method, args...)
	if err != nil {
		return nil, fmt.Errorf("pack %s: %w", method, err)
	}
	nonce, err := c.rpc.PendingNonceAt(ctx, c.opAddr)
	if err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	gas, err := c.rpc.EstimateGas(ctx, ethereum.CallMsg{From: c.opAddr, To: &c.contract, Data: data})
	if err != nil {
		return nil, fmt.Errorf("estimate: %w", err)
	}

	tip, maxFee, err := c.suggestedFee(ctx)
	if err != nil {
		return nil, err
	}

	var lastHash common.Hash
	bumps := 0
	deadline := time.Now().Add(submitTotalBudget)

	send := func() error {
		tx := types.NewTx(&types.DynamicFeeTx{
			ChainID:   c.chainID,
			Nonce:     nonce,
			GasTipCap: tip,
			GasFeeCap: maxFee,
			Gas:       gas,
			To:        &c.contract,
			Data:      data,
		})
		signed, err := types.SignTx(tx, types.LatestSignerForChainID(c.chainID), c.opKey)
		if err != nil {
			return fmt.Errorf("sign: %w", err)
		}
		if err := c.rpc.SendTransaction(ctx, signed); err != nil {
			msg := strings.ToLower(err.Error())
			switch {
			case strings.Contains(msg, "already known"):
				// Same hash already in mempool; fine.
			case strings.Contains(msg, "nonce too low"):
				// A prior submission mined; caller will fetch receipt of lastHash.
				return errNonceMined
			case strings.Contains(msg, "underpriced"):
				return errUnderpriced
			default:
				return fmt.Errorf("send: %w", err)
			}
		}
		lastHash = signed.Hash()
		if onSubmit != nil {
			if err := onSubmit(lastHash); err != nil {
				log.Printf("chain: onSubmit: %v", err)
			}
		}
		return nil
	}

	if err := send(); err != nil {
		if errors.Is(err, errNonceMined) && lastHash != (common.Hash{}) {
			return c.rpc.TransactionReceipt(ctx, lastHash)
		}
		return nil, err
	}

	for time.Now().Before(deadline) {
		attemptCtx, cancel := context.WithTimeout(ctx, submitPerAttempt)
		receipt, werr := c.WaitReceipt(attemptCtx, lastHash)
		cancel()
		if werr == nil && receipt != nil {
			return receipt, nil
		}
		if receipt != nil {
			// Tx reverted on-chain; WaitReceipt returns (receipt, err) here.
			return receipt, werr
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		// Timeout: diagnose whether bumping would actually help.
		if bumps >= submitMaxBumps {
			return nil, fmt.Errorf("%w (last %s, %d bumps used)", ErrTxStuck, lastHash.Hex(), bumps)
		}

		need, reason := c.shouldBump(ctx, tip, maxFee)
		if !need {
			log.Printf("chain: %s pending (%s) — waiting, not bumping", lastHash.Hex(), reason)
			continue
		}

		// Exponential bump (2× the previous tip, recomputed maxFee from
		// current base fee). Aggressive on purpose — each retry doubles
		// our willingness to pay so we converge fast.
		newTip := new(big.Int).Mul(tip, big.NewInt(2))
		newMaxFee, baseErr := c.maxFeeForTip(ctx, newTip)
		if baseErr != nil {
			log.Printf("chain: bump head fetch: %v", baseErr)
			continue
		}
		tip, maxFee = newTip, newMaxFee
		bumps++
		log.Printf("chain: bump %d/%d (%s): tip=%s maxFee=%s", bumps, submitMaxBumps, reason, tip, maxFee)

		if err := send(); err != nil {
			if errors.Is(err, errUnderpriced) {
				// Mempool wanted a bigger jump. Double again and retry the loop iteration.
				tip.Mul(tip, big.NewInt(2))
				continue
			}
			if errors.Is(err, errNonceMined) && lastHash != (common.Hash{}) {
				return c.rpc.TransactionReceipt(ctx, lastHash)
			}
			return nil, err
		}
	}
	return nil, fmt.Errorf("%w after %s", ErrTxStuck, submitTotalBudget)
}

// shouldBump returns whether the current chain state requires us to raise our
// gas to get the tx mined. We only bump when objectively needed — otherwise
// we'd be paying for nothing.
func (c *Client) shouldBump(ctx context.Context, ourTip, ourMaxFee *big.Int) (bool, string) {
	head, err := c.rpc.HeaderByNumber(ctx, nil)
	if err != nil {
		return false, "head fetch failed: " + err.Error()
	}
	if ourMaxFee.Cmp(head.BaseFee) < 0 {
		return true, fmt.Sprintf("maxFee=%s below base fee=%s", ourMaxFee, head.BaseFee)
	}
	suggested, err := c.rpc.SuggestGasTipCap(ctx)
	if err != nil {
		return false, "tip fetch failed: " + err.Error()
	}
	if ourTip.Cmp(suggested) < 0 {
		return true, fmt.Sprintf("tip=%s below suggested=%s", ourTip, suggested)
	}
	return false, "fees still sufficient"
}

func (c *Client) suggestedFee(ctx context.Context) (tip, maxFee *big.Int, err error) {
	tip, err = c.rpc.SuggestGasTipCap(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("gas tip: %w", err)
	}
	maxFee, err = c.maxFeeForTip(ctx, tip)
	return tip, maxFee, err
}

func (c *Client) maxFeeForTip(ctx context.Context, tip *big.Int) (*big.Int, error) {
	head, err := c.rpc.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("head: %w", err)
	}
	return new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), tip), nil
}

var (
	errUnderpriced = errors.New("replacement underpriced")
	errNonceMined  = errors.New("nonce advanced; prior tx mined")
)

// ErrTxStuck is returned when SubmitAndWait gives up after bumping the tx
// `submitMaxBumps` times or running past `submitTotalBudget`. The scheduler
// catches this and emits an `operator.tx_stuck` webhook so the admin can
// step in (the operator's nonce is pinned by the stuck tx; no further pulls
// can succeed until it resolves).
var ErrTxStuck = errors.New("tx stuck after bump attempts")

const (
	submitPerAttempt  = 60 * time.Second  // wait this long for receipt before diagnosing
	submitMaxBumps    = 5                 // hard cap on active fee bumps
	submitTotalBudget = 10 * time.Minute  // overall wall-clock budget per submission
)
