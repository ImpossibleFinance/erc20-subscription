# Checkout sessions

Hosted-checkout API for the erc20-subscription backend. Lets an integrator
mint a one-shot **session**, hand the user a URL, and walk them through a
wallet-funded first month + on-chain approval for renewals. The backend
verifies the on-chain payment, records the recovered wallet on the session,
creates the subscription, and emits a webhook the integrator uses to bind the
wallet to its own user record.

The integrator never types a wallet address into the flow. The wallet is
*discovered* from the EIP-191 signature the user produces at checkout.

---

## Goals

1. **Payment safety.** Before a subscription is created, the backend must
   verify on-chain that the user actually paid month 1 — correct amount,
   correct recipient, correct token, from the same wallet that signed the
   intent. No "trust the browser" anywhere.
2. **Single-wallet rule.** Approve, transfer, and signature must all originate
   from one wallet. Enforced server-side; backstopped in the page UX.
3. **Idempotent + resumable.** A network blip, refresh, or crashed tab must
   not double-charge and must not orphan a subscription.
4. **Integrator-agnostic.** erc20-subs has no concept of "users". The
   integrator passes opaque `metadata` at session-create; the backend echoes it
   back verbatim in the webhook.
5. **Wallet swap = clean restart.** If the user wants to subscribe from a
   different wallet, they mint a new session; the integrator's binding logic
   migrates them. The backend does not try to be clever about cross-session
   wallet changes.

## Non-goals (v1)

- Smart-contract wallets / EIP-1271 signature validation. EOA only.
- Refunds. Out of scope; an off-line operator action.
- Multi-currency. One token per backend, configured at boot.
- Anonymous checkout. Sessions are minted via authenticated admin call from
  the integrator; the public surface is the session URL only.

---

## State machine

```
   POST /sessions
        │
        ▼
   ┌─────────┐  /complete (verification fails)   ┌─────────┐
   │ pending │ ───────────────────────────────▶  │ pending │  (typed error returned;
   └────┬────┘                                   └─────────┘   session stays open)
        │
        │ /complete (verification passes)
        ▼
   ┌───────────┐
   │ completed │  (terminal; subscription created; webhook emitted)
   └───────────┘

   ┌─────────┐  TTL hit (default 15m, configurable)
   │ pending │ ─────────────────────▶  ┌─────────┐
   └─────────┘                         │ expired │  (terminal)
                                       └─────────┘
```

`completed` and `expired` are terminal. There is no `failed` state: a verification
failure returns a typed error on the same `/complete` call and leaves the session
in `pending` so the user can retry.

---

## API

All session endpoints under `/sessions`. Admin endpoints (`/admin/*`) keep the
existing bearer-auth scheme. Session reads + complete are public — the session id
itself is the capability token and the EIP-191 signature is the proof of intent.

### POST /admin/sessions

Admin-authenticated. Mints a session.

```json
// request
{
  "plan_id": "pro_monthly_usdc",
  "success_url": "https://example.com/checkout/ok",          // optional, echoed in /status
  "metadata": { "user_id": "u_01H..." }                       // opaque; max 4 KB JSON
}

// 201 Created
{
  "session_id": "cs_01H8...",
  "expires_at": "2026-06-03T12:15:00Z",
  "checkout_url": "https://<host>/checkout?session=cs_01H8..."  // see "Hosting the page" below
}
```

Validation:
- `plan_id` must exist and be active. 404 otherwise.
- `metadata` size limit: 4 KB serialized. 413 otherwise.
- TTL: `SESSION_TTL` env, default `15m`, clamped `[5m, 60m]`.

### GET /sessions/{id}

Public. Read-only view the checkout page uses to render plan terms + addresses.

```json
{
  "session_id": "cs_01H8...",
  "status": "pending",
  "plan": {
    "id": "pro_monthly_usdc",
    "price_atomic": "49000000",
    "period_count": 1,
    "period_unit": "month"
  },
  "contract_address": "0x89...",
  "token_address":    "0x036C...",
  "token_decimals":   6,
  "token_symbol":     "USDC",
  "treasury_address": "0xd599...",
  "chain_id":         84532,
  "approve_periods":  12,
  "expires_at":       "2026-06-03T12:15:00Z",
  "wallet":           null,                  // populated post-completion
  "subscription_id":  null                   // populated post-completion
}
```

404 on unknown id. If session is `expired`, still returns the body with
`status: "expired"` so the page can render a clear message instead of a 404.

### POST /sessions/{id}/complete

Public. Submitted by the checkout page after the user has approved, paid the
first month, and signed the intent. **The signature is the auth.**

```json
// request
{
  "message":              "erc20-subscription checkout\n\nSession: ...",
  "signature":            "0x...",
  "approval_tx_hash":     "0xabc..." | null,
  "initial_transfer_tx":  "0xdef..."
}

// 200 OK
{
  "session_id":      "cs_01H8...",
  "status":          "completed",
  "wallet":          "0xbeef...",
  "subscription_id": "sub_01H8...",
  "next_charge_at":  "2026-07-03T12:14:30Z"
}
```

See **Verification** below for the precise checks. On any failure, returns
`4xx` with `{ "error": "...", "code": "<typed_code>" }` and the session stays
in `pending`. Once a session has reached `completed`, any further `/complete`
call with the **same** `initial_transfer_tx` returns the same 200 body
(idempotent). A different `initial_transfer_tx` on a completed session is a 409.

### GET /sessions/{id}/status

Public. Cheap polling endpoint — same shape as GET /sessions/{id} but only
returns `{session_id, status, wallet, subscription_id, expires_at}`. Page and
integrator both use this.

---

## EIP-191 challenge

The page constructs the message; the wallet signs it via `personal_sign`. The
backend reconstructs and parses it character-for-character.

```
erc20-subscription checkout

Session: cs_01H8...
Plan: pro_monthly_usdc
Wallet: 0xbeef...
Contract: 0x89edce46cf42666e27a2fa099ee5df25b62736eb
Chain: 84532
Issued: 2026-06-03T12:01:14Z
```

The literal first line is configurable per integrator via `CHALLENGE_PREFIX`
env (default `erc20-subscription checkout`) so the wallet popup can display a
recognizable name to the user. The backend reconstructs the message using its
own configured prefix; a mismatch fails the `challenge_mismatch` check.

Pinned fields (server checks all of these):
- `Session` matches the URL path's session id.
- `Plan` matches the session's plan id.
- `Wallet` (lowercase) matches the address recovered from the signature.
- `Contract` (lowercase) matches the backend's configured Subscriptions contract.
- `Chain` matches the backend's configured chain id.
- `Issued` parses as RFC3339-UTC and is within `CHALLENGE_FRESHNESS`
  (default 10 min) of server time. Future-dated → reject.

Why session id is pinned in the message: prevents replay across sessions. A
signature minted for session A cannot be submitted for session B even if both
target the same plan and wallet.

---

## Verification (`POST /sessions/{id}/complete`)

Order matters: cheap rejections first, on-chain reads last.

### 1. Cheap checks

| step | failure code | notes |
|---|---|---|
| Decode body, reject empty fields | `bad_request` | |
| Session exists, status == `pending` | `session_completed` / `session_expired` / `session_not_found` | terminal-state mismatches return 409 |
| Session not past TTL (race vs. background sweeper) | `session_expired` | |

### 2. Signature checks

| step | failure code |
|---|---|
| Recover signer from `(message, signature)` via EIP-191 personal-sign | `signature_invalid` |
| Parse message; all five pinned fields match | `challenge_mismatch` (with sub-field name in the error string) |
| `Issued` within freshness window | `challenge_expired` |

After this point, `signerWallet = recovered_address.lower()` is fixed. Every
on-chain check below is against `signerWallet`.

### 3. On-chain checks (transfer)

Read `eth_getTransactionByHash` + `eth_getTransactionReceipt` for
`initial_transfer_tx`. Verify all of:

| condition | failure code |
|---|---|
| receipt present | `transfer_not_mined` |
| `receipt.status == 0x1` | `transfer_reverted` |
| `tx.to == TOKEN_ADDR` (case-insensitive) | `transfer_wrong_token` |
| input decodes as `transfer(address,uint256)` | `transfer_wrong_method` |
| decoded `to == TREASURY_ADDR` | `transfer_wrong_recipient` |
| decoded `value == plan.price_atomic` | `transfer_wrong_amount` |
| `tx.from == signerWallet` | `transfer_from_mismatch` |
| `(head_block - receipt.blockNumber) >= MIN_CONFS` | `transfer_not_mined` |

`MIN_CONFS` is chain-tier sensitive: `1` for known testnets (84532, 11155111),
`3` for mainnets. Configurable via `MIN_CONFIRMATIONS` env.

Decoding is done locally — no faith in event logs from the RPC's tracer. The
input bytes are the source of truth (for vanilla USDC) plus the receipt's
`Transfer` log as a cross-check; mismatch between calldata and the
canonical `Transfer(from, to, value)` log → `transfer_log_mismatch` (catches
the exotic case of a reentrant token).

### 4. On-chain checks (allowance)

Read `allowance(signerWallet, contract_address)` at `latest`.

| condition | failure code |
|---|---|
| allowance >= `(approve_periods - 1) * price_atomic` | `insufficient_allowance` |

If `approval_tx_hash` is non-null, also verify its receipt: `receipt.status == 0x1`,
`tx.to == TOKEN_ADDR`, input decodes as `approve(spender, value)` with
`spender == contract_address` and `tx.from == signerWallet`. Mismatches map to
`approval_*` codes mirroring the transfer ones. The allowance read above is the
authoritative gate, however — `approval_tx_hash` is informational (lets us
distinguish "user just approved" from "user reused old allowance").

### 5. Double-spend guard

The session store maintains a unique index on
`(token_address, initial_transfer_tx)`. If a tx hash is already bound to a
different session, reject with `transfer_already_consumed`. The check is
inside the redis lock that wraps `/complete` so two concurrent submissions of
the same hash serialize — only one wins.

### 6. Commit

Inside the same lock:
- Update session: `status = completed`, `wallet = signerWallet`, `initial_transfer_tx`,
  `approval_tx_hash`, `completed_at = now`.
- Create the subscription: `Subscription{address: signerWallet, plan_id,
  start_at: now + plan.period}`. Existing subscription for the same wallet?
  See **Wallet replay below**.
- Enqueue `session.completed` webhook (see Webhook section).
- Return 200.

### Wallet replay (same wallet, fresh session)

If `signerWallet` already has an active subscription on the same `plan_id`:
- We do **not** charge again on-chain — they already paid month 1 with the
  fresh transfer, and the existing scheduler entry is unaware of it.
- Behavior: cancel the old subscription record, create the new one with
  `start_at = now + plan.period`. The user paid; their next charge is one
  period out from now. Old `start_at` is forgotten.
- Emit `session.completed` + `subscription.replaced` webhook events.

This is the "user lost their CLI install and re-subscribed" path. The contract
itself is stateless about subscriptions, so no on-chain action is needed.

If `signerWallet` already has an active subscription on a **different**
`plan_id` (plan switch): same handling — old record dropped, new one created.
Renewal cadence resets.

---

## Single-wallet rule

The recovered signer is the canonical wallet for the whole session. Everything
else is checked against it:

- `transfer.from == signer` (server)
- `approval.from == signer`, when `approval_tx_hash` present (server)
- `allowance(signer, contract) >= renewal_target` (server)

The server is the security boundary. The page exists to save the user from
wasting gas — see the next section for the page-side preflight.

---

## Page-side preflight checks

The server's verification ladder is the boundary, but every `4xx` it returns
is wasted gas, a confused user, and a support ticket. The page MUST run the
full check set locally before opening each wallet popup. If any check fails,
the popup is **not opened** — the page surfaces the problem inline and lets
the user fix it (switch chain, switch wallet, top up balance) before
proceeding.

Persistent state the page holds from connect onward:

- `userAddress` — captured at `eth_requestAccounts`; treated as immutable for
  the lifetime of the page. Any mismatch with `eth_accounts[0]` aborts.
- `session` — the `GET /sessions/{id}` body, refetched if stale (>30s) and
  always refetched after a wallet popup completes (status may have flipped).
- An `accountsChanged` listener that disables the CTA and shows
  "Switch back to `<userAddress>` or refresh to start over with the new wallet."
- A `chainChanged` listener that disables the CTA and tries to switch back via
  `wallet_switchEthereumChain`; if the user refuses, the CTA stays disabled.

### Checks every step shares (run *before* every popup)

| check | recovery if failed |
|---|---|
| `eth_chainId == session.chain_id` (re-read every time) | call `wallet_switchEthereumChain`; on user reject, halt with "Switch to `<chainName>` in your wallet." |
| `eth_accounts[0] == userAddress` (lowercase) | halt with "Switch back to `<userAddress>` in your wallet, or refresh to start over with a new wallet." |
| `session.status == "pending"` (re-fetch GET /sessions/{id}) | halt with the matching terminal-state message (`completed` / `expired`) |
| `now < session.expires_at - 60s` | halt with "Session about to expire — refresh to start over." (60s slack so the user isn't mid-popup when TTL hits) |

If any shared check fails, the page does NOT open the wallet popup. No
`eth_sendTransaction`, no `personal_sign`. The user fixes the condition and
clicks the CTA again.

### Popup 1 — `approve(contract, allowanceTarget)`

Skipped entirely if `existing_allowance >= (approve_periods - 1) * price`. The
page calls `allowance(userAddress, contract)` first and only opens the popup
if the current allowance is short.

Pre-popup checks:

| check | recovery |
|---|---|
| `session.contract_address` and `session.token_address` non-empty | "Server misconfigured — contact support" |
| `eth_getBalance(userAddress)` ≥ rough gas estimate (e.g. 80k gas × current gasPrice × 1.2) | "Not enough `<chainNativeSymbol>` for gas. Top up and retry." |
| `existing_allowance` < target | (otherwise skip popup) |

After the popup returns the tx hash:

| check | recovery |
|---|---|
| `waitForReceipt(hash)` returns `status: 0x1` within timeout | on revert: "Approval reverted on-chain"; on timeout: "Approval not confirmed — retry later" |
| post-mine `allowance(userAddress, contract) >= target` | if still short (rare; user approved a smaller amount in the popup): reopen approve with the missing delta |

### Popup 2 — `USDC.transfer(treasury, plan.price_atomic)`

Pre-popup checks:

| check | recovery |
|---|---|
| `session.treasury_address` non-empty and well-formed (0x + 40 hex) | "Server misconfigured — contact support" |
| `balanceOf(userAddress)` ≥ `plan.price_atomic` (token, not native) | "Not enough `<tokenSymbol>`. Top up and retry." |
| `eth_getBalance(userAddress)` ≥ gas estimate × 1.2 | "Not enough `<chainNativeSymbol>` for gas." |
| popup 1's allowance check still passes (re-read; defends against user revoking between popups) | reopen popup 1 |

Show the user *exactly* what they are about to send before opening the popup:
"Send `<plan.price_human>` `<tokenSymbol>` to `<treasury_address>` (treasury)?"
with a "Cancel" button. The popup itself shows the same calldata; double
display gives the user a chance to spot a phishing iframe overlay.

After the popup returns the tx hash:

| check | recovery |
|---|---|
| `waitForReceipt(hash)` status `0x1` within timeout (90s default) | revert → "Payment reverted on-chain. No `<tokenSymbol>` was moved — retry." (in a true revert no balance moved) |
| decoded receipt `Transfer` log matches `(from=userAddress, to=treasury, value=price)` | mismatch → "Payment didn't match — contact support with tx `<hash>`" (catches the rare case where the wallet auto-rerouted via a custom token) |
| `tx.from == userAddress` (re-read) | mismatch → "Payment came from a different wallet (`<actualFrom>`). Restart with that wallet." |

### Popup 3 — `personal_sign(message)`

The page builds `message` *just before* opening the popup, with a fresh
`Issued: <now-UTC>` timestamp and the session id from the URL. Pre-popup
checks:

| check | recovery |
|---|---|
| Local message matches the exact format from the spec (incl. all five pinned fields and exact line breaks) | bug; never user-facing |
| `Wallet` line is `userAddress.toLowerCase()` | bug |
| `Contract` line matches `session.contract_address.toLowerCase()` | bug |
| `Chain` line matches `session.chain_id` (int) | bug |

After the popup returns the signature:

| check | recovery |
|---|---|
| Local EIP-191 recover of `(message, signature)` equals `userAddress` (cheap library call) | wallet returned a sig for a different account — alert: "Signature came from a different wallet. Switch back to `<userAddress>` or restart." |

### POST `/complete`

By the time the page calls `/complete`, every server-side check should pass.
If it returns a `4xx`, treat it as a bug on the page's side (the local
preflight missed something) and surface the typed error so it can be fixed.
The page should NOT swallow `4xx` and silently retry — it should show the
typed message from the **Failure-mode codes** table.

Exception: `transfer_not_mined` (HTTP 425) is the one "retry without user
action" case. Poll every 5s for up to 60s before surfacing to the user.

### Summary

Net effect: a user who switches wallet, switches chain, or runs low on
balance is told **before** they open a popup, not after they've spent gas. A
user on a misconfigured server is told before they pay. The server's
verification ladder is now a last-line defense rather than the routine
gatekeeper.

---

## Failure-mode codes

| code | HTTP | page action |
|---|---|---|
| `bad_request` | 400 | bug; surface as-is |
| `session_not_found` | 404 | "Session not found." → "Start over" |
| `session_expired` | 410 | "Session expired." → "Start over" |
| `session_completed` | 409 | "Already subscribed." → close |
| `signature_invalid` | 401 | retry sign |
| `challenge_mismatch` | 401 | retry sign (rebuilds message from current state) |
| `challenge_expired` | 401 | retry sign |
| `transfer_not_mined` | 425 | poll every 5s up to ~60s; show "Waiting for confirmation…" |
| `transfer_reverted` | 422 | "Payment reverted on-chain." → "Start over" |
| `transfer_wrong_token` | 422 | unrecoverable; "Contact support, tx \<hash\>" |
| `transfer_wrong_method` | 422 | unrecoverable; same |
| `transfer_wrong_recipient` | 422 | unrecoverable; same |
| `transfer_wrong_amount` | 422 | unrecoverable; same |
| `transfer_from_mismatch` | 422 | "You signed with A but paid from B. Switch back to A, or start over with B." |
| `transfer_already_consumed` | 409 | "That payment is already used by another subscription." → "Start over" |
| `transfer_log_mismatch` | 422 | unrecoverable; contact support |
| `insufficient_allowance` | 422 | "Approve N more for renewals" → reopen approve popup → re-POST |
| `approval_*` | 422 | mirror of transfer_*; same actions |

`425 Too Early` for `transfer_not_mined` is intentional — it signals "retry
later", and the page treats it specifically.

---

## Idempotency

- **Same session id + same `initial_transfer_tx`**: returns the same completed
  body. Safe to retry on network flake or page refresh after success.
- **Same session id + different `initial_transfer_tx` on a completed session**:
  409 `session_completed`. We do not re-bill.
- **Different session id + same `initial_transfer_tx`**: 409
  `transfer_already_consumed`. Prevents the "pay once, claim twice" attack
  where a user opens two checkout tabs.

The unique index lives in the store interface:

```go
// store.go additions
type CheckoutSession struct {
    ID                string
    PlanID            string
    Status            string  // "pending" | "completed" | "expired"
    Wallet            string  // lowercase; "" until completed
    InitialTransferTx string  // lowercase 0x hash; "" until verified
    ApprovalTxHash    string
    Metadata          json.RawMessage  // opaque integrator payload
    SuccessURL        string
    ExpiresAt         time.Time
    CompletedAt       *time.Time
    CreatedAt         time.Time
}

type SessionStore interface {
    Create(ctx, *CheckoutSession) error
    Get(ctx, id) (*CheckoutSession, error)
    // Complete is a CAS: only updates if the session is currently pending
    // AND no other session has this initial_transfer_tx. Returns
    // ErrTxAlreadyConsumed on the latter, ErrConflictState on the former.
    Complete(ctx, id, wallet, transferTx, approvalTx string, at time.Time) error
    ExpirePending(ctx, before time.Time) (int, error)
}
```

The redis impl uses `WATCH/MULTI/EXEC` around the
`session:{id}` hash and the `tx:{tokenAddr}:{txHash}` reservation key.

---

## Webhook: `session.completed`

Emitted exactly once per session, after the commit lock releases. Retried on
non-2xx by the existing webhooks/dispatch loop.

```json
{
  "id":         "evt_01H8...",
  "type":       "session.completed",
  "created_at": "2026-06-03T12:02:55Z",
  "data": {
    "session_id":           "cs_01H8...",
    "plan_id":              "pro_monthly_usdc",
    "wallet":               "0xbeef...",
    "subscription_id":      "sub_01H8...",
    "initial_transfer_tx":  "0xdef...",
    "approval_tx_hash":     "0xabc...",
    "next_charge_at":       "2026-07-03T12:14:30Z",
    "metadata":             { "user_id": "u_01H..." }
  }
}
```

`subscription.replaced` (when an existing wallet re-subscribes through a
session) carries `{ wallet, old_subscription_id, new_subscription_id,
plan_id, metadata }`.

The existing `subscription.charged`, `subscription.allowance_low`, etc.
continue to fire as before for ongoing renewals.

---

## Hosting the checkout page

The backend serves a minimal hosted page at `/checkout` that reads
`?session=<id>` from the URL, fetches `GET /sessions/{id}`, runs the 3-popup
flow, and POSTs `/complete`. Integrators can override `success_url` at
session-create to redirect after completion; the page falls back to closing
the tab if `success_url` is empty.

An integrator can either link straight to the erc20-subs-hosted page or
reverse-proxy `/checkout` + `/sessions/*` through their own domain. Both work
— the page is same-origin to whatever serves it, and the API calls go to
erc20-subs either directly or via the integrator's proxy.

**Resume on page death.** When the transfer tx is broadcast, the page stashes
`{session_id, transfer_tx}` in `localStorage` keyed by session id. If the user
re-opens the same URL within the TTL, the page detects the stashed entry,
skips the approve+transfer popups, and goes straight to `personal_sign` +
`/complete`. If the TTL has passed, the stashed entry is cleared and the user
starts a new session (their previous payment is still on-chain; recovery is
an operator action — see below).

---

## Operator recovery (out-of-band)

For the "user paid month 1, then closed the tab and abandoned the session"
case, the operator can call:

```
POST /admin/sessions/{id}/force_complete
  body: { "initial_transfer_tx": "0x..." }
```

Runs the same verification as `/complete`, minus the signature check (admin
auth replaces it). Used by support when a user paid but never finished the
flow. Emits `session.completed` with a `recovered_by: "operator"` marker in
`data`.

Not exposed to the public checkout flow; this is purely a support tool.

---

## Background sweeper

A goroutine in the backend runs every minute:
- Marks any `pending` session past `expires_at` as `expired`.
- Releases the `tx:{tokenAddr}:{txHash}` reservation for any **never-verified**
  session that expires (so an unrelated user who happens to land on the same
  hash later — extraordinarily unlikely — isn't blocked forever by a ghost
  reservation). Verified hashes stay reserved permanently.

---

## Configuration

New env vars on the backend:

| var | default | meaning |
|---|---|---|
| `SESSION_TTL` | `15m` | session lifetime; `[5m, 60m]` |
| `CHALLENGE_FRESHNESS` | `10m` | EIP-191 `Issued` window |
| `MIN_CONFIRMATIONS` | `1` testnet / `3` mainnet | block depth required for tx checks |
| `APPROVE_PERIODS_HINT` | `12` | hint surfaced on `GET /sessions/{id}` so the page knows the target allowance |
| `CHECKOUT_PAGE_ENABLED` | `true` | turn off the embedded page if the integrator hosts its own |

Existing token/contract/treasury/chain values are reused.

---

## Open questions

1. Should `metadata` be returned on `GET /sessions/{id}`? Probably not — that
   endpoint is public, and integrators may put PII-adjacent ids in metadata.
   Default: omit from public reads, include in webhooks + admin reads.
2. Mainnet `MIN_CONFIRMATIONS`: 3 is a starting point; revisit after first
   real-money pilot.
3. Should the page POST directly to erc20-subs or through the integrator?
   Both work. Direct is simpler; integrator-proxied lets the integrator log
   the verification result for support tickets. Default: direct, integrator
   can flip via reverse-proxy if they want.
