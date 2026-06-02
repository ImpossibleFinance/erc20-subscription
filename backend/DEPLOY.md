# Deploying the subscription backend

Runbook for deploying this service on ifhost. Adapt for any container host.

## What you need before starting

- The Subscriptions contract deployed on-chain (see `contracts/`). Have its address.
- A **persistent Redis** with AOF enabled. Upstash, Redis Cloud, a sidecar instance — your choice, but it MUST survive restarts. Sub state lives here; losing it = double-charge risk.
- A dedicated **operator EOA** with some ETH for gas. This is the hot key that signs `pull()` txs. Generate fresh; do not reuse owner/treasury.
- 32 random bytes for `ADMIN_TOKEN` and 32 for `WEBHOOK_SECRET`. `openssl rand -hex 32`.

## Deploy

```bash
cd backend
ifhost deploy
```

Pick `if-subs` as the app name when prompted (or whatever you'd like — match
the integrator's `SUBS_BACKEND_URL`).

After the first deploy, set the runtime config:

```bash
ifhost secrets set \
  RPC_URL="https://sepolia.base.org" \
  CHAIN_ID="84532" \
  CONTRACT_ADDR="0x89EDCe46cF42666e27a2fA099Ee5Df25B62736Eb" \
  TOKEN_ADDR="0x036CbD53842c5426634e7929541eC2318f3dCF7e" \
  OPERATOR_KEY_HEX="$(your operator key, no 0x)" \
  REDIS_URL="rediss://default:<password>@<host>:<port>" \
  ADMIN_TOKEN="$(openssl rand -hex 32)" \
  WEBHOOK_URL="https://host.impossi.build/webhooks/subscriptions" \
  WEBHOOK_SECRET="$(openssl rand -hex 32)"
```

Save the `ADMIN_TOKEN` and `WEBHOOK_SECRET` somewhere safe — you'll set the same values on the integrator (impossible-host) as `SUBS_ADMIN_TOKEN` and `SUBS_WEBHOOK_SECRET`.

## Register the three plans

After the service is up:

```bash
SUBS=https://if-subs.impossi.build
ADMIN=<your ADMIN_TOKEN>

for p in \
  '{"id":"hobby_monthly_usdc","price_atomic":"15000000","period_count":1,"period_unit":"month","active":true}' \
  '{"id":"pro_monthly_usdc","price_atomic":"49000000","period_count":1,"period_unit":"month","active":true}' \
  '{"id":"team_monthly_usdc","price_atomic":"149000000","period_count":1,"period_unit":"month","active":true}'
do
  curl -s -X POST "$SUBS/admin/plans" \
    -H "Authorization: Bearer $ADMIN" \
    -H "Content-Type: application/json" \
    -d "$p"
  echo
done
```

(Prices match the existing ifhost plans: Hobby $15, Pro $49, Team $149.)

Verify:

```bash
curl -s "$SUBS/plans" | jq
```

## Configure ifhost

On the impossible-host side, set:

```bash
ifhost secrets set --app <your-ifhost-app> \
  SUBS_BACKEND_URL="https://if-subs.impossi.build" \
  SUBS_ADMIN_TOKEN="<same as above>" \
  SUBS_WEBHOOK_SECRET="<same as above>" \
  SUBS_CONTRACT_ADDRESS="0x89EDCe46cF42666e27a2fA099Ee5Df25B62736Eb" \
  SUBS_TOKEN_ADDRESS="0x036CbD53842c5426634e7929541eC2318f3dCF7e"
```

Redeploy ifhost so the new env vars take effect.

## Smoke test

1. Visit `https://host.impossi.build/crypto-checkout` with a wallet that has Base Sepolia USDC.
2. The page should populate with the three plans (proxied via `/subscription/crypto/plans` → `if-subs.impossi.build/plans`).
3. Connect wallet → pick a plan → click "Approve & subscribe".
4. Wallet prompts to switch to Base Sepolia (auto-add if not present).
5. First popup: `USDC.approve(contract, 12 × plan.price)`.
6. After confirmation, the page POSTs to `/subscription/crypto/subscribe`, which forwards to subs-backend.
7. Within a tick (60s default), the scheduler pulls the first charge.
8. `Charged` event lands on-chain.
9. Webhook fires to ifhost → the user's `PlanExpiresAt` extends by 30d and they're promoted from `free` to the bought tier.

Check via:
```bash
# Subs-backend's view of the subscription
curl -s "$SUBS/subscriptions/<your-wallet>" | jq

# ifhost's view of the user's plan (replace with your usual GET)
curl -s -H "Authorization: Bearer <user-token>" https://host.impossi.build/subscription | jq
```

## What to watch for

- `operator.gas_low` webhook → top up the operator EOA.
- `operator.tx_stuck` webhook → check the in-flight tx hash on basescan; likely a sequencer issue. Don't bump manually; the scheduler bumps when needed.
- `subscription.payment_failed` for many users at once → could indicate a chain-side problem (e.g. USDC paused, your contract paused). Check before assuming all users underfunded.
- Redis disk usage growing — make sure AOF rewrite is happening; `BGREWRITEAOF` if not.
