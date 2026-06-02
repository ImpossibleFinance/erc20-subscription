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
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/impossiblefinance/erc20-subscription/backend/internal/store"
)

type API struct {
	store      store.Store
	adminToken string
}

func New(s store.Store, adminToken string) *API {
	return &API{store: s, adminToken: adminToken}
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
	Address      string    `json:"address"`
	PlanID       string    `json:"plan_id"`
	StartAt      time.Time `json:"start_at,omitempty"` // optional; default = now
}

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
	plan, err := a.store.GetPlan(r.Context(), req.PlanID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if plan == nil || !plan.Active {
		writeErr(w, http.StatusBadRequest, "plan not found or inactive")
		return
	}
	start := req.StartAt
	if start.IsZero() {
		start = time.Now().UTC()
	}
	sub := &store.Subscription{
		User:          normalizeAddr(req.Address),
		PlanID:        req.PlanID,
		Status:        store.StatusActive,
		NextAttemptAt: start,
		CreatedAt:     time.Now().UTC(),
	}
	if err := validateAddress(sub.User); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.store.UpsertSubscription(r.Context(), sub); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sub)
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
