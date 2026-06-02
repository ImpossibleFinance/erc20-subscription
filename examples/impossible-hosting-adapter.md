# Integrating with impossible-hosting

`impossible-hosting` is the reference integrator: a Go web service whose users
hold a `PlanExpiresAt` timestamp. When the subscription backend emits
`subscription.charged`, we extend that timestamp by the plan's period.

This document shows the minimal wiring — same approach works for any web
service.

## 1. Plan ↔ tier mapping

Decide which on-chain plans correspond to which tiers in your app:

```go
var planToTier = map[string]string{
    "hobby_monthly_usdc": "hobby",
    "pro_monthly_usdc":   "pro",
    "team_monthly_usdc":  "team",
}
```

The plan IDs are the human strings you registered via `POST /admin/plans` on
the subscription backend. The on-chain ID is `keccak256(humanID)`.

## 2. Webhook handler

```go
package handler

import (
    "crypto/hmac"
    "crypto/sha256"
    "encoding/hex"
    "encoding/json"
    "io"
    "net/http"
    "time"
)

type subEvent struct {
    ID   string `json:"id"`
    Type string `json:"type"`
    Data struct {
        User     string `json:"user"`
        PlanID   string `json:"plan_id"`
        TxHash   string `json:"tx_hash"`
        Block    uint64 `json:"block"`
    } `json:"data"`
}

func (h *Handler) SubscriptionWebhook(w http.ResponseWriter, r *http.Request) {
    body, _ := io.ReadAll(r.Body)

    // Verify signature in constant time.
    mac := hmac.New(sha256.New, []byte(h.cfg.WebhookSecret))
    mac.Write(body)
    want := mac.Sum(nil)
    got, _ := hex.DecodeString(r.Header.Get("X-Signature"))
    if !hmac.Equal(got, want) {
        http.Error(w, "bad signature", http.StatusUnauthorized)
        return
    }

    var ev subEvent
    if err := json.Unmarshal(body, &ev); err != nil {
        http.Error(w, "bad json", http.StatusBadRequest)
        return
    }

    // Idempotency: skip duplicates by event ID. 24h TTL is enough; the
    // subscription backend's retry budget is exhausted long before that.
    seen, _ := h.store.SetNX(r.Context(), "subwebhook:"+ev.ID, "1", 24*time.Hour)
    if !seen {
        w.WriteHeader(http.StatusOK)
        return
    }

    tier := planToTier[ev.Data.PlanID]
    if tier == "" {
        // Unknown plan — log and 200 (4xx would cause retries).
        w.WriteHeader(http.StatusOK)
        return
    }

    user, _ := h.store.GetUserByWalletAddress(r.Context(), ev.Data.User)
    if user == nil {
        w.WriteHeader(http.StatusOK)
        return
    }

    switch ev.Type {
    case "subscription.charged":
        // Extend by period (looked up from cached plan config).
        period := h.plans[ev.Data.PlanID].Period
        newExpiry := maxTime(time.Now(), user.PlanExpiresAt).Add(period)
        h.store.UpdateUserPlan(r.Context(), user.ID, tier, newExpiry)
    case "subscription.cancelled", "subscription.deactivated":
        // Let it run out naturally — don't downgrade immediately.
    }

    w.WriteHeader(http.StatusOK)
}

func maxTime(a, b time.Time) time.Time {
    if a.After(b) {
        return a
    }
    return b
}
```

## 3. Wallet binding

`impossible-hosting` already has a wallet-binding flow (`/auth/wallet/bind`)
from the deposit-address top-up rail. The same binding works here — when a
user subscribes, the `Subscribed` event carries their wallet address; you look
up their `userID` via the existing `binding:<address>` index.

If you don't have a binding flow yet:
1. Generate a small random verify amount (e.g. 0.001 USDC).
2. Have the user send it to your treasury before subscribing.
3. The deposit watcher binds wallet → userID.
4. From then on, all subscription events carry that wallet and you can match.

## 4. Endpoints to add to impossible-hosting

- `POST /webhooks/subscriptions` — the handler above
- `GET /subscription/crypto/plans` — proxy to backend `/plans` so the CLI/UI
  can show what's available
- CLI: `ifhost sub subscribe --plan <id>` — print the wallet-side ops the user
  needs to run (approve + subscribe), with a copyable EthersJS / viem snippet

## 5. Coexistence with card billing

Card billing (LemonSqueezy) and crypto subscriptions write to the same
`PlanExpiresAt`. The rule we use:

- Card subscription active → ignore crypto events that would *downgrade* (we
  don't want a missed crypto charge to cancel a card-funded sub).
- Card subscription cancelled → crypto becomes the source of truth.

Implement that as a guard in the webhook handler before calling
`UpdateUserPlan`.
