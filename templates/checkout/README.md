# Checkout template

A single-file drop-in checkout page for the subscription backend. Host it on
your own domain. Zero dependencies — just `window.ethereum`. Works with
MetaMask, Rabby, Coinbase Wallet, Frame, and other extension wallets out of
the box.

## Hosting

Drop `index.html` anywhere static:

- S3 + CloudFront
- Vercel / Netlify
- GitHub Pages
- A `/checkout` route in your existing app
- Even `python -m http.server` for local testing

There's no build step. The page loads, fetches plans, and runs.

## Configuration

Edit the `CONFIG` block at the top of `index.html`:

```js
const CONFIG = {
  contractAddress: "0x…",                  // your deployed Subscriptions contract
  tokenAddress:    "0x833589…02913",       // ERC-20 the contract pulls (Base USDC default)
  tokenSymbol:     "USDC",
  tokenDecimals:   6,
  chainId:         8453,                    // 8453 Base, 84532 Base Sepolia
  chainName:       "Base",
  rpcURL:          "https://mainnet.base.org",
  blockExplorer:   "https://basescan.org",
  plansURL:        "https://subs.your-domain.example/plans",  // public CORS-open endpoint
  subscribeURL:    "/api/subscribe",        // YOUR backend (see below)
  approvePeriods:  12,
}
```

## What your backend needs to expose

The template POSTs to `subscribeURL` after the wallet approves USDC.
**This must be your own backend** — never put the subscription backend's
`ADMIN_TOKEN` in client code.

Your backend's `/api/subscribe` endpoint should:

1. **Authenticate the user** (session cookie, JWT, whatever you already use).
2. **Verify the wallet binding** — confirm the request's `address` is the one
   the user is allowed to bill (e.g. they signed in with that wallet, or you
   ran a separate verify-deposit flow earlier).
3. **Optionally verify the approval tx** — fetch
   `eth_getTransactionReceipt(approval_tx_hash)` to confirm the user actually
   approved before you spend an admin call.
4. **Forward to the subscription backend** with your admin token:

```http
POST https://subs.your-domain.example/admin/subscriptions
Authorization: Bearer $ADMIN_TOKEN
Content-Type: application/json

{ "address": "0x…", "plan_id": "pro_monthly" }
```

5. **Return 2xx on success, 4xx/5xx with an error body on failure.** The
   template surfaces the error body to the user.

## Mobile / WalletConnect

The template uses `window.ethereum` directly, which means desktop browser
extensions only. For mobile wallets, drop in
[`@reown/appkit`](https://reown.com/appkit) (formerly WalletConnect AppKit)
or [`wagmi`](https://wagmi.sh) — they expose the same Web3 RPC under the
hood, so the rest of the flow is unchanged.

## Customization

The whole page is ~250 lines including styling. Edit the CSS variables at the
top of the `<style>` block for theming, or replace the markup wholesale —
the only load-bearing bits are:

- `fetchPlans()` calls `GET ${plansURL}` and renders them
- The approval flow calls `eth_sendTransaction` with an ABI-encoded
  `approve(spender, amount)` (selector `0x095ea7b3`)
- `POST ${subscribeURL}` is sent with `{ plan_id, address, approval_tx_hash }`

## What happens after subscribe

The subscription backend's scheduler picks up the new sub on its next tick
(default 60s) and starts pulling. Your backend receives webhook events:

- `subscription.allowance_required` — fires immediately on subscribe; use to
  confirm the user approved enough
- `subscription.charged` — each successful cycle pull
- `subscription.allowance_low` — heads-up when their approval will run out
- `subscription.payment_failed`, `subscription.cancelled` — exception paths

See the main README for the full event reference.
