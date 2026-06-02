# Deploying the subscription backend

The service runs as a single Go binary or container. Distroless image is
built by the included `Dockerfile`; deploy it on whatever container host
you already use (Fly.io, Cloud Run, ECS, k8s, a plain VM with systemd —
the binary doesn't care).

This document is hosting-agnostic. For ifhost-specific or k8s-specific
recipes, layer them on top.

## What you need before starting

- The Subscriptions contract deployed on-chain (see `contracts/`). Have its address.
- A **persistent Redis** with AOF enabled. Upstash, Redis Cloud, a sidecar
  instance, your own VM — your choice, but it MUST survive restarts. Sub
  state lives here; losing it = double-charge risk.
- A dedicated **operator EOA** with some ETH for gas. This is the hot key
  that signs `pull()` txs. Generate fresh; do not reuse owner/treasury.
- 32 random bytes for `ADMIN_TOKEN` and 32 for `WEBHOOK_SECRET`:
  `openssl rand -hex 32`.

## Build

```bash
cd backend
docker build -t erc20-subs .
```

Or natively: `go build -o bin/server ./cmd/server`.

## Run

```bash
docker run -d --name erc20-subs --restart=always -p 8080:8080 \
  -e RPC_URL="https://sepolia.base.org" \
  -e CHAIN_ID="84532" \
  -e CONTRACT_ADDR="0x…" \
  -e TOKEN_ADDR="0x036CbD53842c5426634e7929541eC2318f3dCF7e" \
  -e OPERATOR_KEY_HEX="<your operator key, no 0x>" \
  -e REDIS_URL="rediss://default:<password>@<host>:<port>" \
  -e ADMIN_TOKEN="$(openssl rand -hex 32)" \
  -e WEBHOOK_URL="https://your-integrator.example/webhooks/subscriptions" \
  -e WEBHOOK_SECRET="$(openssl rand -hex 32)" \
  erc20-subs
```

Save `ADMIN_TOKEN` and `WEBHOOK_SECRET` somewhere safe — your integrator
needs the same values to make admin calls and verify webhook signatures.

For most hosts the values above go into your platform's secret store, not
a literal `docker run`. See your platform's docs for `--secret`, `secrets
set`, or equivalent.

## Register plans

After the service is up:

```bash
SUBS=https://subs.your-domain.example
ADMIN=<your ADMIN_TOKEN>

curl -s -X POST "$SUBS/admin/plans" \
  -H "Authorization: Bearer $ADMIN" \
  -H "Content-Type: application/json" \
  -d '{"id":"pro_monthly_usdc","price_atomic":"49000000","period_count":1,"period_unit":"month","active":true}'
```

`price_atomic` is in the token's smallest unit. USDC has 6 decimals, so
`49000000` = $49.

Verify: `curl -s "$SUBS/plans" | jq`.

## Smoke test

1. Host the included `templates/checkout/index.html` somewhere with the
   matching `contractAddress`, `tokenAddress`, `chainId`. Or run the
   checkout directly against `localhost`.
2. Connect a wallet, pick a plan, approve.
3. Within the tick interval (default 60s), the scheduler pulls the first
   charge. Watch your `WEBHOOK_URL` endpoint for `subscription.charged`.

## What to watch

- `operator.gas_low` webhook → top up the operator EOA.
- `operator.tx_stuck` webhook → check the in-flight tx hash on a block
  explorer; usually a sequencer issue. The scheduler bumps fees when the
  diagnostic says they're underpriced; don't bump manually.
- `subscription.payment_failed` for many users at once → could indicate
  a chain-side problem (token paused, contract paused). Check before
  assuming all users underfunded.
- Redis disk usage growing — make sure AOF rewrite is happening
  (`BGREWRITEAOF` if not).
