// Package api exposes HTTP endpoints.
//
// Public reads (CORS-open):
//
//	GET  /plans                          list plans
//	GET  /plans/{id}                     single plan
//	GET  /subscriptions/{address}        sub state for a wallet
//	GET  /healthz
//
// Admin (Bearer ADMIN_TOKEN):
//
//	POST /admin/plans                          upsert plan
//	POST /admin/subscriptions                  create a subscription
//	GET  /admin/subscriptions                  list (optional ?status=)
//	POST /admin/subscriptions/{addr}/cancel    mark cancelled
//
// User-side on-chain actions (token.approve, contract pull/cancel) happen
// from the user's wallet on the frontend — never via this backend.
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/impossiblefinance/erc20-subscription/backend/internal/chain"
	"github.com/impossiblefinance/erc20-subscription/backend/internal/store"
	"github.com/impossiblefinance/erc20-subscription/backend/internal/webhooks"
)

type API struct {
	store              store.Store
	chain              *chain.Client
	webhook            *webhooks.Sender
	adminToken         string
	tokenAddr          common.Address
	treasuryAddr       common.Address
	tokenSymbol        string
	tokenDecimals      uint8
	allowanceLowMonths int

	// Checkout-sessions config (see docs/checkout-sessions.md). Zero values
	// fall back to safe defaults but the production path should always wire
	// these from internal/config.
	challengePrefix    string
	challengeFreshness time.Duration
	sessionTTL         time.Duration
	minConfirmations   uint64
	approvePeriodsHint int

	now func() time.Time
}

type Deps struct {
	Chain              *chain.Client
	Webhook            *webhooks.Sender
	TokenAddr          common.Address
	TreasuryAddr       common.Address
	TokenSymbol        string
	TokenDecimals      uint8
	AllowanceLowMonths int

	ChallengePrefix    string
	ChallengeFreshness time.Duration
	SessionTTL         time.Duration
	MinConfirmations   uint64
	ApprovePeriodsHint int
}

func New(s store.Store, adminToken string, d Deps) *API {
	if d.ChallengePrefix == "" {
		d.ChallengePrefix = "erc20-subscription checkout"
	}
	if d.ChallengeFreshness == 0 {
		d.ChallengeFreshness = 10 * time.Minute
	}
	if d.SessionTTL == 0 {
		d.SessionTTL = 15 * time.Minute
	}
	if d.ApprovePeriodsHint == 0 {
		d.ApprovePeriodsHint = 12
	}
	return &API{
		store:              s,
		chain:              d.Chain,
		webhook:            d.Webhook,
		adminToken:         adminToken,
		tokenAddr:          d.TokenAddr,
		treasuryAddr:       d.TreasuryAddr,
		tokenSymbol:        d.TokenSymbol,
		tokenDecimals:      d.TokenDecimals,
		allowanceLowMonths: d.AllowanceLowMonths,
		challengePrefix:    d.ChallengePrefix,
		challengeFreshness: d.ChallengeFreshness,
		sessionTTL:         d.SessionTTL,
		minConfirmations:   d.MinConfirmations,
		approvePeriodsHint: d.ApprovePeriodsHint,
		now:                func() time.Time { return time.Now().UTC() },
	}
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /plans", a.cors(http.HandlerFunc(a.listPlans)))
	mux.Handle("GET /plans/{id}", a.cors(http.HandlerFunc(a.getPlan)))
	mux.Handle("GET /subscriptions/{address}", a.cors(http.HandlerFunc(a.getSub)))
	mux.Handle("OPTIONS /plans", a.cors(http.HandlerFunc(noop)))
	mux.Handle("OPTIONS /plans/{id}", a.cors(http.HandlerFunc(noop)))
	mux.Handle("OPTIONS /subscriptions/{address}", a.cors(http.HandlerFunc(noop)))

	mux.Handle("POST /admin/plans", a.requireAdmin(http.HandlerFunc(a.upsertPlan)))
	mux.Handle("POST /admin/subscriptions", a.requireAdmin(http.HandlerFunc(a.createSub)))
	mux.Handle("GET /admin/subscriptions", a.requireAdmin(http.HandlerFunc(a.listSubs)))
	mux.Handle("POST /admin/subscriptions/{address}/cancel", a.requireAdmin(http.HandlerFunc(a.cancelSub)))

	// Hosted-checkout sessions. Public read + complete; admin mint +
	// force-complete. See docs/checkout-sessions.md.
	mux.Handle("POST /admin/sessions", a.requireAdmin(http.HandlerFunc(a.createSession)))
	mux.Handle("POST /admin/sessions/{id}/force_complete", a.requireAdmin(http.HandlerFunc(a.forceCompleteSession)))
	mux.Handle("GET /sessions/{id}", a.cors(http.HandlerFunc(a.getSession)))
	mux.Handle("GET /sessions/{id}/status", a.cors(http.HandlerFunc(a.getSessionStatus)))
	mux.Handle("POST /sessions/{id}/complete", a.corsPost(http.HandlerFunc(a.completeSession)))
	mux.Handle("OPTIONS /sessions/{id}", a.cors(http.HandlerFunc(noop)))
	mux.Handle("OPTIONS /sessions/{id}/status", a.cors(http.HandlerFunc(noop)))
	mux.Handle("OPTIONS /sessions/{id}/complete", a.corsPost(http.HandlerFunc(noop)))

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

func (a *API) cors(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Max-Age", "86400")
		h.ServeHTTP(w, r)
	})
}

// corsPost mirrors cors but allows POST. Used by the public checkout-session
// `/complete` endpoint, which the page submits cross-origin.
func (a *API) corsPost(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Max-Age", "86400")
		h.ServeHTTP(w, r)
	})
}

func noop(http.ResponseWriter, *http.Request) {}

func (a *API) requireAdmin(h http.Handler) http.Handler {
	expected := sha256.Sum256([]byte(a.adminToken))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		gotHash := sha256.Sum256([]byte(got))
		if subtle.ConstantTimeCompare(expected[:], gotHash[:]) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// ---------- public ----------

func (a *API) listPlans(w http.ResponseWriter, r *http.Request) {
	plans, err := a.store.ListPlans(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plans": plans})
}

func (a *API) getPlan(w http.ResponseWriter, r *http.Request) {
	p, err := a.store.GetPlan(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if p == nil {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *API) getSub(w http.ResponseWriter, r *http.Request) {
	sub, err := a.store.GetSubscription(r.Context(), normalizeAddr(r.PathValue("address")))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if sub == nil {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, sub)
}

// ---------- admin ----------

type upsertPlanReq struct {
	ID          string `json:"id"`
	PriceAtomic string `json:"price_atomic"`
	PeriodCount uint32 `json:"period_count"`
	PeriodUnit  string `json:"period_unit"` // "day" | "month" | "year"
	Active      bool   `json:"active"`
}

func (a *API) upsertPlan(w http.ResponseWriter, r *http.Request) {
	var req upsertPlanReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		writeErr(w, http.StatusBadRequest, "id, price_atomic, period_count, period_unit required")
		return
	}
	if _, ok := new(big.Int).SetString(req.PriceAtomic, 10); !ok {
		writeErr(w, http.StatusBadRequest, "price_atomic must be a base-10 integer string")
		return
	}
	if req.PeriodCount == 0 {
		writeErr(w, http.StatusBadRequest, "period_count must be > 0")
		return
	}
	if !store.ValidPeriodUnit(req.PeriodUnit) {
		writeErr(w, http.StatusBadRequest, "period_unit must be day, month, or year")
		return
	}
	plan := &store.Plan{
		ID:          req.ID,
		PriceAtomic: req.PriceAtomic,
		PeriodCount: req.PeriodCount,
		PeriodUnit:  req.PeriodUnit,
		Active:      req.Active,
		CreatedAt:   time.Now().UTC(),
	}
	if err := a.store.UpsertPlan(r.Context(), plan); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

type createSubReq struct {
	Address string    `json:"address"`
	PlanID  string    `json:"plan_id"`
	StartAt time.Time `json:"start_at,omitempty"` // optional; default = now
}

// createSub is the SINGLE endpoint for subscribe + plan-change. Behavior:
//
//   - No existing sub for `address`: create one. NextAttemptAt = start_at or now.
//   - Existing sub, same plan: idempotent (touches updated_at only).
//   - Existing sub, different plan:
//   - Compute prorated diff = (new_price - old_price) × fraction-of-cycle-remaining.
//     Downgrades produce a non-positive diff and are ignored (no refunds).
//   - Stash the diff in PendingChargeAtomic so the scheduler pulls it on
//     the next tick — BEFORE the next regular cycle charge.
//   - Swap PlanID; keep NextAttemptAt so the regular billing anchor doesn't shift.
//
// In all cases, after the sub is written we emit `subscription.allowance_required`
// so the integrator can prompt the user to top up their USDC approval to cover
// the next 12 periods of the (possibly new) plan. This is the "messy" case
// the integrator must handle once, not constantly.
func (a *API) createSub(w http.ResponseWriter, r *http.Request) {
	var req createSubReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Address == "" || req.PlanID == "" {
		writeErr(w, http.StatusBadRequest, "address and plan_id required")
		return
	}
	addr := normalizeAddr(req.Address)
	if err := validateAddress(addr); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	newPlan, err := a.store.GetPlan(r.Context(), req.PlanID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if newPlan == nil || !newPlan.Active {
		writeErr(w, http.StatusBadRequest, "plan not found or inactive")
		return
	}

	existing, err := a.store.GetSubscription(r.Context(), addr)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	sub, changed := a.applyPlan(r.Context(), existing, newPlan, addr, req.StartAt)
	if err := a.store.UpsertSubscription(r.Context(), sub); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	if changed {
		// Best-effort: tell the integrator how much allowance the user should
		// approve to cover the (possibly new) plan for the next 12 periods.
		// Done in a goroutine so the API responds fast — the webhook is
		// retry-on-5xx anyway.
		go a.emitAllowanceRequired(context.Background(), sub, newPlan)
	}
	writeJSON(w, http.StatusOK, sub)
}

// applyPlan merges a (possibly new) plan request into the existing sub (or
// creates a fresh one). Returns the merged sub and whether anything material
// changed (new sub or plan swap), which gates the allowance_required webhook.
func (a *API) applyPlan(ctx context.Context, existing *store.Subscription, newPlan *store.Plan, addr string, startAt time.Time) (*store.Subscription, bool) {
	now := a.now()
	if startAt.IsZero() {
		startAt = now
	}

	if existing == nil {
		return &store.Subscription{
			User:          addr,
			PlanID:        newPlan.ID,
			Status:        store.StatusActive,
			NextAttemptAt: startAt,
			CreatedAt:     now,
		}, true
	}

	// Same plan: just refresh status if needed; no proration.
	if existing.PlanID == newPlan.ID {
		if existing.Status == store.StatusCancelled {
			existing.Status = store.StatusActive
			existing.NextAttemptAt = startAt
			return existing, true
		}
		return existing, false
	}

	// Plan change. Compute proration diff IF we have enough history to know
	// where in the cycle we are (last_charged_at + next_attempt_at). On a
	// brand-new sub that's never been charged, skip proration — there's
	// nothing the user has overpaid for.
	oldPlanID := existing.PlanID
	oldPlan, _ := a.store.GetPlan(ctx, oldPlanID)
	diff := proratedDiff(now, existing, oldPlan, newPlan)
	if diff != nil && diff.Sign() > 0 {
		existing.PendingChargeAtomic = diff.String()
	}
	existing.PlanID = newPlan.ID
	existing.Status = store.StatusActive
	existing.DunningAttempts = 0
	existing.LastError = ""
	return existing, true
}

// proratedDiff returns max(0, (new_price - old_price) × remaining/cycle) in
// the token's atomic units. Returns nil if proration can't be computed (no
// LastChargedAt, plans missing, etc.).
func proratedDiff(now time.Time, sub *store.Subscription, oldPlan, newPlan *store.Plan) *big.Int {
	if oldPlan == nil || newPlan == nil {
		return nil
	}
	if sub.LastChargedAt.IsZero() || !sub.NextAttemptAt.After(sub.LastChargedAt) {
		return nil
	}
	cycle := sub.NextAttemptAt.Sub(sub.LastChargedAt)
	remaining := sub.NextAttemptAt.Sub(now)
	if remaining <= 0 {
		return nil
	}
	oldPrice, ok1 := new(big.Int).SetString(oldPlan.PriceAtomic, 10)
	newPrice, ok2 := new(big.Int).SetString(newPlan.PriceAtomic, 10)
	if !ok1 || !ok2 {
		return nil
	}
	priceDiff := new(big.Int).Sub(newPrice, oldPrice)
	if priceDiff.Sign() <= 0 {
		return nil // downgrade or same; no charge
	}
	// diff = priceDiff * remaining / cycle  (integer math)
	num := new(big.Int).Mul(priceDiff, big.NewInt(int64(remaining)))
	denom := big.NewInt(int64(cycle))
	return new(big.Int).Quo(num, denom)
}

// emitAllowanceRequired computes how much USDC the user should approve
// (12 periods of the current plan + any pending one-time charge) and sends
// the webhook. Best-effort: failures are logged.
func (a *API) emitAllowanceRequired(ctx context.Context, sub *store.Subscription, plan *store.Plan) {
	if a.webhook == nil {
		return
	}
	periods := a.allowanceLowMonths * 6 // recommend covering ~6× the warn threshold
	if periods < 12 {
		periods = 12
	}
	price, ok := new(big.Int).SetString(plan.PriceAtomic, 10)
	if !ok {
		return
	}
	required := new(big.Int).Mul(price, big.NewInt(int64(periods)))
	if sub.PendingChargeAtomic != "" {
		if pending, ok := new(big.Int).SetString(sub.PendingChargeAtomic, 10); ok {
			required = new(big.Int).Add(required, pending)
		}
	}

	payload := map[string]any{
		"user":                     sub.User,
		"plan_id":                  sub.PlanID,
		"required_allowance_atomic": required.String(),
		"recommended_periods":      periods,
		"price_atomic":             plan.PriceAtomic,
		"period_count":             plan.PeriodCount,
		"period_unit":              plan.PeriodUnit,
	}
	if sub.PendingChargeAtomic != "" {
		payload["prorated_charge_atomic"] = sub.PendingChargeAtomic
	}
	if a.chain != nil && (a.tokenAddr != common.Address{}) {
		if cur, err := a.chain.Allowance(ctx, a.tokenAddr, common.HexToAddress(sub.User), a.chain.Contract()); err == nil {
			payload["current_allowance_atomic"] = cur.String()
			topup := new(big.Int).Sub(required, cur)
			if topup.Sign() < 0 {
				topup.SetInt64(0)
			}
			payload["top_up_atomic"] = topup.String()
		}
	}
	if err := a.webhook.Send(ctx, webhooks.EventAllowanceRequired, payload); err != nil {
		log.Printf("api: allowance_required webhook: %v", err)
	}
}

func (a *API) listSubs(w http.ResponseWriter, r *http.Request) {
	subs, err := a.store.ListSubscriptions(r.Context(), r.URL.Query().Get("status"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscriptions": subs})
}

func (a *API) cancelSub(w http.ResponseWriter, r *http.Request) {
	addr := normalizeAddr(r.PathValue("address"))
	sub, err := a.store.GetSubscription(r.Context(), addr)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if sub == nil {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	sub.Status = store.StatusCancelled
	sub.NextAttemptAt = time.Now().AddDate(100, 0, 0)
	if err := a.store.UpsertSubscription(r.Context(), sub); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sub)
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func normalizeAddr(h string) string {
	return strings.ToLower(common.HexToAddress(h).Hex())
}

func validateAddress(addr string) error {
	if !strings.HasPrefix(addr, "0x") || len(addr) != 42 {
		return errors.New("address must be 0x-prefixed 42-char hex")
	}
	return nil
}
