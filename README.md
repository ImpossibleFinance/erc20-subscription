# erc20-subscription

A **lean, self-hostable recurring-payment stack** for any ERC-20 token, working with any regular wallet (EOA, MetaMask, hardware wallets — no smart wallet required). Think "Stripe for crypto subscriptions," but you run it.

Two pieces:

- **`contracts/`** — a ~50-line Solidity contract. Users approve it once on the payment token; the operator pulls. No upgrades, no proxies, no on-chain plan state.
- **`backend/`** — a Go service that schedules pulls, runs dunning, watches each user's remaining allowance, and emits HMAC-signed webhooks. Redis-backed; one binary.

Designed to drop into any web service that wants Web3-native recurring billing. Integrates cleanly with [impossible-hosting](https://github.com/ImpossibleFinance/impossible-hosting) via webhooks.

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
| `subscription.charged`        | A pull succeeded on-chain | `{user, plan_id, amount_atomic, period_count, period_unit, tx_hash, block, next_attempt}` |
| `subscription.payment_failed` | Pull reverted; scheduler will retry per dunning policy | `{user, plan_id, amount_atomic, attempts, next_attempt, reason}` |
| `subscription.cancelled`      | Dunning exhausted OR admin cancel | `{user, plan_id, reason}` |
| `subscription.allowance_low`  | After a successful pull, remaining allowance covers fewer than `ALLOWANCE_LOW_MONTHS` more periods | `{user, plan_id, allowance_atomic, price_atomic, remaining_periods, threshold_periods}` |

5xx is retried up to 3× with linear backoff. 4xx is terminal.

### Upgrades / downgrades

`POST /admin/subscriptions` is upsert — calling it again for the same `address` with a different `plan_id` swaps the plan:

```jsonc
// User upgrades from "pro" ($10) to "team" ($50) on 2026-06-01, mid-cycle:
// → charge new price immediately
POST /admin/subscriptions
{ "address": "0x…", "plan_id": "team" }

// → charge new price at end of currently-paid-for cycle
POST /admin/subscriptions
{ "address": "0x…", "plan_id": "team", "start_at": "2026-06-15T00:00:00Z" }
```

Charge history (`last_charged_tx`, `last_charged_block`) is preserved across the swap.

**Allowance and upgrades:** the user's USDC allowance is on the contract, not the plan. If the user approved `12 × $10 = $120` for Pro and upgrades to Team ($50), only 2 more pulls fit before the allowance runs out. The next `subscription.allowance_low` webhook will fire as usual — your app prompts the user to re-approve a higher amount for the new tier.

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
