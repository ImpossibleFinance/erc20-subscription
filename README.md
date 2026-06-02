# erc20-subscription

A **lean, self-hostable recurring-payment stack** for any ERC-20 token, working with any regular wallet (EOA, MetaMask, hardware wallets — no smart wallet required). Think "Stripe for crypto subscriptions," but you run it.

Two pieces:

- **`contracts/`** — a ~50-line Solidity contract. Users approve it once on the payment token; the operator pulls. No upgrades, no proxies, no on-chain plan state.
- **`backend/`** — a Go service that schedules pulls, runs dunning, watches each user's remaining allowance, and emits HMAC-signed webhooks. Redis-backed; one binary.

Designed to drop into any web service that wants Web3-native recurring billing.

---

## How it works

```
┌──────────────┐                 ┌─────────────────┐               ┌────────────┐
│ your web app │                 │ subscription    │               │  on-chain  │
│              │                 │ backend (this)  │               │  contract  │
└──────┬───────┘                 └────────┬────────┘               └─────┬──────┘
       │ POST /admin/plans               │                              │
       │ (id, price_atomic, period)      │                              │
       │ ────────────────────────────▶ │                              │
       │                                  │                              │
       │ user clicks "Subscribe to Pro"   │                              │
       │ POST /admin/subscriptions       │                              │
       │ (address, plan_id)              │                              │
       │ ────────────────────────────▶ │                              │
       │                                  │                              │
       │  show user: "approve N USDC"     │                              │
       │  (recommended: 12 × plan.price)  │                              │
       │  user wallet ── USDC.approve(contract, N) ──────────────────▶│
       │                                  │                              │
       │                                  │ each period, scheduler:      │
       │                                  │ ── pull(user, amount) ─────▶│
       │                                  │ ◀── Charged event ──────────│
       │ ◀── POST /webhook ──────────────│                              │
       │  (subscription.charged)         │                              │
       │                                  │                              │
       │  near the end of approval:       │                              │
       │ ◀── POST /webhook ──────────────│                              │
       │  (subscription.allowance_low)   │                              │
       │  → email user to re-approve     │                              │
```

The contract is **just a permissioned ERC-20 puller** — no plan state, no on-chain subscriptions, no cadence enforcement. All of that is the backend's job. The user's USDC allowance is the hard limit on how much the operator can ever take.

---

## Contract

`contracts/src/Subscriptions.sol` — ~50 lines.

| Role | Who | What they can do |
|---|---|---|
| `owner` | **Multisig on prod** | rotate the operator (use `address(0)` as emergency halt) |
| `operator` | Hot key the backend signs with | `pull(user, amount)` |
| `treasury` | **Immutable** | receives all pulled funds |
| user | any wallet | `token.approve(contract, N)` to authorize, `token.approve(contract, 0)` to revoke |

Design choices:
- **No on-chain plans.** Storing prices on-chain duplicates the backend's source of truth and adds attack surface for zero benefit.
- **No subscribe/cancel functions.** Subscribe = user calls `token.approve`. Cancel = `token.approve(_, 0)` or hit the backend's cancel API. Both are zero-line additions to the contract.
- **No `paused` flag.** The owner can `setOperator(address(0))` for the same effect. One lever instead of two.
- **Immutable treasury.** Locks the destination in the user-visible bytecode forever — users can verify it once and never have to re-trust.
- **`charge()` rate limiting is the user's USDC approval.** We recommend ~12 months of plan price; the user controls the ceiling.

Build & test:

```bash
cd contracts
forge install foundry-rs/forge-std
forge test -vv
```

11 tests pass.

Deploy (example for Base mainnet, USDC):

```bash
forge create src/Subscriptions.sol:Subscriptions \
  --rpc-url $RPC_URL \
  --private-key $OWNER_KEY \
  --constructor-args 0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913 $TREASURY $OPERATOR
```

---

## Backend

Single Go binary. Run `cmd/server`. Configuration via env vars (see `backend/.env.example`).

```bash
cd backend
go build -o bin/server ./cmd/server
RPC_URL=https://mainnet.base.org \
CHAIN_ID=8453 \
CONTRACT_ADDR=0x… \
TOKEN_ADDR=0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913 \
OPERATOR_KEY_HEX=… \
REDIS_URL=redis://localhost:6379/0 \
ADMIN_TOKEN=$(openssl rand -hex 32) \
WEBHOOK_URL=https://your-app.example/webhooks/subscriptions \
WEBHOOK_SECRET=$(openssl rand -hex 32) \
./bin/server
```

Default `TICK_INTERVAL=60s`. Default dunning: 24h → 72h → 168h, then cancel. Default `ALLOWANCE_LOW_MONTHS=2`.

### Admin REST API

All endpoints require `Authorization: Bearer $ADMIN_TOKEN`.

| Method | Path | Body | Notes |
|---|---|---|---|
| `POST` | `/admin/plans` | `{id, price_atomic, period_count, period_unit, active}` | Define / update a plan. `period_unit` is `"day"`, `"month"`, or `"year"`; months use calendar arithmetic (Jan 31 → Feb 28 → Mar 31 → ...). |
| `POST` | `/admin/subscriptions` | `{address, plan_id, start_at?}` | Upsert. New address → new sub. Existing address → swap the plan (charge history preserved). Use `start_at` to control when the new price kicks in (default: now). |
| `GET`  | `/admin/subscriptions?status=active\|past_due\|cancelled` | — | List. |
| `POST` | `/admin/subscriptions/{address}/cancel` | — | Stop scheduling. The user's USDC approval is untouched (they should revoke it for a hard chain-level cutoff). |

Public reads (no auth, CORS-open):

| Method | Path | Notes |
|---|---|---|
| `GET` | `/plans` | List active plans. Safe for frontends. |
| `GET` | `/plans/{id}` | Single plan. |
| `GET` | `/subscriptions/{address}` | Wallet's sub state. Safe for frontends — addresses are public. |
| `GET` | `/healthz` | Liveness. |

### Webhook events

Every event: HMAC-SHA256-signed body, header `X-Signature: <hex>`. Envelope:

```json
{
  "id":         "evt_8a3f…",
  "type":       "subscription.charged",
  "created_at": "2026-06-01T12:00:00Z",
  "data":       { ... }
}
```

| `type` | When | `data` |
|---|---|---|
| `subscription.charged`            | A regular-cycle pull succeeded on-chain | `{user, plan_id, amount_atomic, period_count, period_unit, tx_hash, block, next_attempt}` |
| `subscription.prorated_charge`    | A one-time pull succeeded (mid-cycle upgrade diff)      | `{user, plan_id, amount_atomic, tx_hash, block}` |
| `subscription.payment_failed`     | A pull reverted; scheduler will retry per dunning policy | `{user, plan_id, amount_atomic, attempts, next_attempt, reason}` |
| `subscription.cancelled`          | Dunning exhausted OR admin cancel                       | `{user, plan_id, reason}` |
| `subscription.allowance_low`      | After a successful pull, remaining allowance covers fewer than `ALLOWANCE_LOW_MONTHS` more periods | `{user, plan_id, allowance_atomic, price_atomic, remaining_periods, threshold_periods}` |
| `subscription.allowance_required` | New sub OR plan change — tells your app how much to prompt the user to approve | `{user, plan_id, required_allowance_atomic, current_allowance_atomic, top_up_atomic, recommended_periods, price_atomic, period_count, period_unit, prorated_charge_atomic?}` |
| `operator.gas_low`                | Operator EOA's ETH balance won't cover at least `OPERATOR_GAS_BUFFER_MONTHS` more months of pulls at current gas — top it up | `{operator, balance_wei, threshold_wei, buffer_months, pulls_in_horizon}` |
| `operator.tx_stuck`               | A pull tx didn't mine within the budget (10 min, 5 fee bumps) — operator nonce is pinned; admin must inspect | `{operator, user, plan_id, in_flight_tx, reason}` |

5xx is retried up to 3× with linear backoff. 4xx is terminal.

### Upgrades / downgrades

`POST /admin/subscriptions` is the single endpoint for new subs AND plan changes. The backend does the bookkeeping:

```jsonc
// Mid-cycle upgrade from Pro ($10/mo) to Team ($50/mo):
POST /admin/subscriptions
{ "address": "0x…", "plan_id": "team" }
```

What happens on a plan change:

1. **Prorated diff is queued.** If the new plan costs more, the backend computes `(new_price − old_price) × remaining_cycle_fraction` and stashes it on the sub as a one-time charge. Example: 17 days left of a 31-day cycle, Pro → Team → diff ≈ $21.94 queued.
2. **Plan is swapped.** `next_attempt_at` (the regular billing anchor) is unchanged — the user's cycle date stays consistent.
3. **`subscription.allowance_required` webhook fires immediately.** Tells your app exactly how much USDC the user should approve to cover the new tier (12 periods + any pending diff). Your app prompts the user to call `USDC.approve(contract, required_allowance_atomic)`.
4. **Once allowance covers it, the scheduler pulls the diff** and emits `subscription.prorated_charge`. Then the regular cycle continues on its existing anchor, charging the new price.

Downgrades skip steps 1 and 4 (no refunds): just swap the plan, fire `allowance_required` so the user can shrink their approval if they want. The next cycle charges the lower price.

Charge history (`last_charged_tx`, `last_charged_block`) is preserved across plan swaps.

### Stuck transactions

The operator EOA has a single nonce sequence — only one pull tx in flight at a time. The submitter does NOT blindly bump fees on every timeout. After each 60-second wait without a receipt, it asks one specific question:

> Has the chain's current `baseFee` or `suggestedTip` risen above what we paid?

- **Yes** (we underbid) → double the tip, recompute `maxFeePerGas`, resubmit at the same nonce.
- **No** (our fee is still competitive) → just keep waiting. The tx is fine; the network is slow, the sequencer is congested, or the mempool is moving slow. Bumping here would burn gas for nothing.

Bumping is **exponential** (2× per retry) and **capped at 5 active bumps** with a **10-minute total wall-clock budget per pull**. After either cap, the backend gives up and emits `operator.tx_stuck` with the tx hash so the admin can investigate. No more pulls run until the stuck tx clears or the admin rotates the operator key.

### Operator-side vs user-side failures

The scheduler classifies pull errors:

- **User-side** (insufficient allowance, blocked transfer, etc.): standard dunning — `subscription.payment_failed` webhook, retry at 24h → 72h → 168h, then `subscription.cancelled`.
- **Operator-side** (out of gas, RPC outage, contract paused, nonce mismatch): no dunning, `DunningAttempts` is NOT incremented. Sub backs off 5 minutes and retries. The admin sees `operator.gas_low` / `operator.tx_stuck` instead.

This keeps users from being cancelled because *we* dropped the ball.

---

## Frontend integration

Two on-chain calls — both from the user's wallet:

```ts
import { parseAbi } from 'viem'

const USDC = '0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913'
const SUBS = '0x…' // your deployed Subscriptions contract

// 1. Approve USDC (recommend 12 × plan.price so they get a year of charges).
await walletClient.writeContract({
  address: USDC,
  abi: parseAbi(['function approve(address,uint256) returns (bool)']),
  functionName: 'approve',
  args: [SUBS, 12n * 10_000_000n], // 12 × $10
})

// 2. (Optional, signals cancel to backend.)
await walletClient.writeContract({
  address: SUBS,
  abi: parseAbi(['function cancel()']),
  functionName: 'cancel',
})
```

The Subscribed step lives entirely on your backend — call `POST /admin/subscriptions` after authenticating the user.

---

## Security checklist (prod)

- [ ] Owner is a multisig (Safe / similar). Never an EOA on mainnet.
- [ ] Operator is a dedicated hot key, no other privileges anywhere, funded with enough ETH for ~year of charges.
- [ ] `WEBHOOK_SECRET` and `ADMIN_TOKEN` are 32+ random bytes from a secret manager.
- [ ] Treasury is a cold wallet or the same multisig.
- [ ] After deploying, run `forge verify-contract` so Etherscan/Basescan renders the source.
- [ ] Monitor: operator ETH balance, Charged event rate, in-flight tx age. Alert if charges stop.
- [ ] Recommend users approve only ~12 months of plan price — re-approval is the safety budget against operator compromise.

---

## Project layout

```
contracts/
  src/Subscriptions.sol            ~50 lines, the entire on-chain surface
  test/Subscriptions.t.sol         11 tests
  test/MockERC20.sol
backend/
  cmd/server/main.go               binary entry point
  internal/
    api/                           HTTP API
    chain/                         go-ethereum wrapper, ABI, event decode, allowance lookup
    config/                        env loading
    dunning/                       retry policy
    scheduler/                     find-due → pull → confirm → bookkeeping
    store/                         Redis + in-memory impls; calendar period math
    webhooks/                      HMAC-signed POSTs
```

---

## License

MIT.
