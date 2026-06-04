package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	ethhexutil "github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/impossiblefinance/erc20-subscription/backend/internal/chain"
	"github.com/impossiblefinance/erc20-subscription/backend/internal/store"
	"github.com/impossiblefinance/erc20-subscription/backend/internal/webhooks"
)

// Typed error codes returned in the JSON body of 4xx responses on the
// session endpoints. The page maps each to a concrete user-visible action;
// see docs/checkout-sessions.md → "Failure-mode codes".
const (
	codeBadRequest             = "bad_request"
	codeSessionNotFound        = "session_not_found"
	codeSessionExpired         = "session_expired"
	codeSessionCompleted       = "session_completed"
	codeSignatureInvalid       = "signature_invalid"
	codeChallengeMismatch      = "challenge_mismatch"
	codeChallengeExpired       = "challenge_expired"
	codeTransferNotMined       = "transfer_not_mined"
	codeTransferReverted       = "transfer_reverted"
	codeTransferWrongToken     = "transfer_wrong_token"
	codeTransferWrongMethod    = "transfer_wrong_method"
	codeTransferWrongRecipient = "transfer_wrong_recipient"
	codeTransferWrongAmount    = "transfer_wrong_amount"
	codeTransferFromMismatch   = "transfer_from_mismatch"
	codeTransferAlreadyConsumed = "transfer_already_consumed"
	codeTransferLogMismatch    = "transfer_log_mismatch"
	codeInsufficientAllowance  = "insufficient_allowance"
	codeApprovalReverted       = "approval_reverted"
	codeApprovalWrongToken     = "approval_wrong_token"
	codeApprovalWrongMethod    = "approval_wrong_method"
	codeApprovalWrongSpender   = "approval_wrong_spender"
	codeApprovalFromMismatch   = "approval_from_mismatch"
	codeUpstreamError          = "upstream_error"
)

// ---------- POST /admin/sessions ----------

type createSessionReq struct {
	PlanID     string          `json:"plan_id"`
	SuccessURL string          `json:"success_url,omitempty"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
}

type createSessionResp struct {
	SessionID   string    `json:"session_id"`
	ExpiresAt   time.Time `json:"expires_at"`
	CheckoutURL string    `json:"checkout_url,omitempty"`
}

const metadataMaxBytes = 4 * 1024

func (a *API) createSession(w http.ResponseWriter, r *http.Request) {
	var req createSessionReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.PlanID == "" {
		writeSessionErr(w, http.StatusBadRequest, codeBadRequest, "plan_id required")
		return
	}
	if len(req.Metadata) > metadataMaxBytes {
		writeSessionErr(w, http.StatusRequestEntityTooLarge, codeBadRequest, "metadata exceeds 4 KB")
		return
	}
	plan, err := a.store.GetPlan(r.Context(), req.PlanID)
	if err != nil {
		writeSessionErr(w, http.StatusInternalServerError, codeUpstreamError, err.Error())
		return
	}
	if plan == nil || !plan.Active {
		writeSessionErr(w, http.StatusNotFound, codeBadRequest, "plan not found or inactive")
		return
	}

	now := a.now()
	sess := &store.CheckoutSession{
		ID:         newSessionID(),
		PlanID:     plan.ID,
		Status:     store.SessionStatusPending,
		Metadata:   req.Metadata,
		SuccessURL: req.SuccessURL,
		CreatedAt:  now,
		ExpiresAt:  now.Add(a.sessionTTL),
	}
	if err := a.store.CreateSession(r.Context(), sess); err != nil {
		writeSessionErr(w, http.StatusInternalServerError, codeUpstreamError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, createSessionResp{
		SessionID:   sess.ID,
		ExpiresAt:   sess.ExpiresAt,
		CheckoutURL: a.checkoutURLFor(r, sess.ID),
	})
}

// checkoutURLFor builds the absolute checkout URL based on the request host.
// Lets integrators reverse-proxy `/checkout` through their own domain without
// the backend having to know its own external URL.
func (a *API) checkoutURLFor(r *http.Request, id string) string {
	scheme := "https"
	if r.TLS == nil && !strings.HasPrefix(r.Host, "localhost") && r.Header.Get("X-Forwarded-Proto") == "" {
		scheme = "http"
	}
	if fp := r.Header.Get("X-Forwarded-Proto"); fp != "" {
		scheme = fp
	}
	return scheme + "://" + r.Host + "/checkout?session=" + id
}

// ---------- GET /sessions/{id} ----------

type sessionPublicResp struct {
	SessionID        string    `json:"session_id"`
	Status           string    `json:"status"`
	Plan             planView  `json:"plan"`
	ContractAddress  string    `json:"contract_address"`
	TokenAddress     string    `json:"token_address"`
	TokenDecimals    uint8     `json:"token_decimals"`
	TokenSymbol      string    `json:"token_symbol"`
	TreasuryAddress  string    `json:"treasury_address"`
	ChainID          int64     `json:"chain_id"`
	ApprovePeriods   int       `json:"approve_periods"`
	ChallengePrefix  string    `json:"challenge_prefix"`
	ExpiresAt        time.Time `json:"expires_at"`
	Wallet           string    `json:"wallet,omitempty"`
	SubscriptionID   string    `json:"subscription_id,omitempty"`
	SuccessURL       string    `json:"success_url,omitempty"`
}

type planView struct {
	ID          string `json:"id"`
	PriceAtomic string `json:"price_atomic"`
	PeriodCount uint32 `json:"period_count"`
	PeriodUnit  string `json:"period_unit"`
}

func (a *API) getSession(w http.ResponseWriter, r *http.Request) {
	sess, plan, ok := a.loadSessionWithPlan(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, a.sessionPublicView(sess, plan))
}

func (a *API) sessionPublicView(sess *store.CheckoutSession, plan *store.Plan) sessionPublicResp {
	return sessionPublicResp{
		SessionID: sess.ID,
		Status:    sess.Status,
		Plan: planView{
			ID:          plan.ID,
			PriceAtomic: plan.PriceAtomic,
			PeriodCount: plan.PeriodCount,
			PeriodUnit:  plan.PeriodUnit,
		},
		ContractAddress: strings.ToLower(a.chain.Contract().Hex()),
		TokenAddress:    strings.ToLower(a.tokenAddr.Hex()),
		TokenDecimals:   a.tokenDecimals,
		TokenSymbol:     a.tokenSymbol,
		TreasuryAddress: strings.ToLower(a.treasuryAddr.Hex()),
		ChainID:         a.chain.ChainID(),
		ApprovePeriods:  a.approvePeriodsHint,
		ChallengePrefix: a.challengePrefix,
		ExpiresAt:       sess.ExpiresAt,
		Wallet:          sess.Wallet,
		SubscriptionID:  sess.SubscriptionID,
		SuccessURL:      sess.SuccessURL,
	}
}

// loadSessionWithPlan reads the session (refreshing its status if past TTL),
// fetches the plan, and writes any error response. On a non-OK return, the
// response is already written.
func (a *API) loadSessionWithPlan(w http.ResponseWriter, r *http.Request) (*store.CheckoutSession, *store.Plan, bool) {
	id := r.PathValue("id")
	sess, err := a.store.GetSession(r.Context(), id)
	if err != nil {
		writeSessionErr(w, http.StatusInternalServerError, codeUpstreamError, err.Error())
		return nil, nil, false
	}
	if sess == nil {
		writeSessionErr(w, http.StatusNotFound, codeSessionNotFound, "session not found")
		return nil, nil, false
	}
	// Soft-expire: if the sweeper hasn't flipped it yet, surface as expired
	// rather than pending.
	if sess.Status == store.SessionStatusPending && !sess.ExpiresAt.After(a.now()) {
		sess.Status = store.SessionStatusExpired
	}
	plan, err := a.store.GetPlan(r.Context(), sess.PlanID)
	if err != nil {
		writeSessionErr(w, http.StatusInternalServerError, codeUpstreamError, err.Error())
		return nil, nil, false
	}
	if plan == nil {
		writeSessionErr(w, http.StatusInternalServerError, codeUpstreamError, "plan vanished")
		return nil, nil, false
	}
	return sess, plan, true
}

// ---------- GET /sessions/{id}/status ----------

type sessionStatusResp struct {
	SessionID      string    `json:"session_id"`
	Status         string    `json:"status"`
	Wallet         string    `json:"wallet,omitempty"`
	SubscriptionID string    `json:"subscription_id,omitempty"`
	ExpiresAt      time.Time `json:"expires_at"`
}

func (a *API) getSessionStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := a.store.GetSession(r.Context(), id)
	if err != nil {
		writeSessionErr(w, http.StatusInternalServerError, codeUpstreamError, err.Error())
		return
	}
	if sess == nil {
		writeSessionErr(w, http.StatusNotFound, codeSessionNotFound, "session not found")
		return
	}
	if sess.Status == store.SessionStatusPending && !sess.ExpiresAt.After(a.now()) {
		sess.Status = store.SessionStatusExpired
	}
	writeJSON(w, http.StatusOK, sessionStatusResp{
		SessionID:      sess.ID,
		Status:         sess.Status,
		Wallet:         sess.Wallet,
		SubscriptionID: sess.SubscriptionID,
		ExpiresAt:      sess.ExpiresAt,
	})
}

// ---------- POST /sessions/{id}/complete ----------

type completeSessionReq struct {
	Message            string `json:"message"`
	Signature          string `json:"signature"`
	ApprovalTxHash     string `json:"approval_tx_hash,omitempty"`
	InitialTransferTx  string `json:"initial_transfer_tx"`
}

type completeSessionResp struct {
	SessionID      string    `json:"session_id"`
	Status         string    `json:"status"`
	Wallet         string    `json:"wallet"`
	SubscriptionID string    `json:"subscription_id"`
	NextChargeAt   time.Time `json:"next_charge_at"`
}

func (a *API) completeSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req completeSessionReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeSessionErr(w, http.StatusBadRequest, codeBadRequest, "invalid json")
		return
	}
	if req.Message == "" || req.Signature == "" || req.InitialTransferTx == "" {
		writeSessionErr(w, http.StatusBadRequest, codeBadRequest, "message, signature, initial_transfer_tx required")
		return
	}

	// 1. Cheap checks: session exists and is pending.
	sess, err := a.store.GetSession(r.Context(), id)
	if err != nil {
		writeSessionErr(w, http.StatusInternalServerError, codeUpstreamError, err.Error())
		return
	}
	if sess == nil {
		writeSessionErr(w, http.StatusNotFound, codeSessionNotFound, "session not found")
		return
	}
	if sess.Status == store.SessionStatusCompleted {
		// Idempotent replay: same transfer tx → same result.
		if strings.EqualFold(sess.InitialTransferTx, req.InitialTransferTx) {
			plan, _ := a.store.GetPlan(r.Context(), sess.PlanID)
			next := time.Time{}
			if plan != nil && sess.CompletedAt != nil {
				next = plan.AddPeriod(*sess.CompletedAt)
			}
			writeJSON(w, http.StatusOK, completeSessionResp{
				SessionID:      sess.ID,
				Status:         sess.Status,
				Wallet:         sess.Wallet,
				SubscriptionID: sess.SubscriptionID,
				NextChargeAt:   next,
			})
			return
		}
		writeSessionErr(w, http.StatusConflict, codeSessionCompleted, "session already completed with a different payment")
		return
	}
	if sess.Status != store.SessionStatusPending || !sess.ExpiresAt.After(a.now()) {
		writeSessionErr(w, http.StatusGone, codeSessionExpired, "session expired")
		return
	}

	// 2. Signature checks.
	signer, err := RecoverPersonalSigner(req.Message, req.Signature)
	if err != nil {
		writeSessionErr(w, http.StatusUnauthorized, codeSignatureInvalid, err.Error())
		return
	}
	challenge, err := ParseChallenge(req.Message)
	if err != nil {
		writeSessionErr(w, http.StatusUnauthorized, codeChallengeMismatch, err.Error())
		return
	}
	if challenge.Prefix != a.challengePrefix {
		writeSessionErr(w, http.StatusUnauthorized, codeChallengeMismatch, "prefix mismatch")
		return
	}
	if challenge.SessionID != sess.ID {
		writeSessionErr(w, http.StatusUnauthorized, codeChallengeMismatch, "session_id mismatch")
		return
	}
	if challenge.PlanID != sess.PlanID {
		writeSessionErr(w, http.StatusUnauthorized, codeChallengeMismatch, "plan_id mismatch")
		return
	}
	if !strings.EqualFold(challenge.Wallet, signer.Hex()) {
		writeSessionErr(w, http.StatusUnauthorized, codeChallengeMismatch, "wallet mismatch")
		return
	}
	if !strings.EqualFold(challenge.Contract, a.chain.Contract().Hex()) {
		writeSessionErr(w, http.StatusUnauthorized, codeChallengeMismatch, "contract mismatch")
		return
	}
	if challenge.ChainID != a.chain.ChainID() {
		writeSessionErr(w, http.StatusUnauthorized, codeChallengeMismatch, "chain mismatch")
		return
	}
	age := a.now().Sub(challenge.Issued)
	if age < 0 || age > a.challengeFreshness {
		writeSessionErr(w, http.StatusUnauthorized, codeChallengeExpired, "challenge timestamp outside freshness window")
		return
	}

	// 3. On-chain transfer verification.
	plan, err := a.store.GetPlan(r.Context(), sess.PlanID)
	if err != nil || plan == nil {
		writeSessionErr(w, http.StatusInternalServerError, codeUpstreamError, "plan lookup failed")
		return
	}
	expectedPrice, ok := new(big.Int).SetString(plan.PriceAtomic, 10)
	if !ok {
		writeSessionErr(w, http.StatusInternalServerError, codeUpstreamError, "plan price malformed")
		return
	}

	transferHash := normalizeHash(req.InitialTransferTx)
	if !isHashLike(transferHash) {
		writeSessionErr(w, http.StatusBadRequest, codeBadRequest, "initial_transfer_tx is not a 0x-prefixed 32-byte hash")
		return
	}
	fetched, err := a.chain.Fetch(r.Context(), common.HexToHash(transferHash))
	if err == chain.ErrTxNotMined {
		writeSessionErr(w, http.StatusTooEarly, codeTransferNotMined, "transfer not yet mined")
		return
	}
	if err != nil {
		writeSessionErr(w, http.StatusBadGateway, codeUpstreamError, err.Error())
		return
	}
	if fetched.Receipt.Status != 1 {
		writeSessionErr(w, http.StatusUnprocessableEntity, codeTransferReverted, "transfer tx reverted on-chain")
		return
	}
	if fetched.Tx.To() == nil || *fetched.Tx.To() != a.tokenAddr {
		writeSessionErr(w, http.StatusUnprocessableEntity, codeTransferWrongToken, "transfer is not against the configured token")
		return
	}
	to, value, decoded := chain.DecodeTransferCall(fetched.Tx.Data())
	if !decoded {
		writeSessionErr(w, http.StatusUnprocessableEntity, codeTransferWrongMethod, "calldata is not ERC-20 transfer()")
		return
	}
	if to != a.treasuryAddr {
		writeSessionErr(w, http.StatusUnprocessableEntity, codeTransferWrongRecipient, "transfer recipient is not the contract treasury")
		return
	}
	if value.Cmp(expectedPrice) != 0 {
		writeSessionErr(w, http.StatusUnprocessableEntity, codeTransferWrongAmount, "transfer amount does not match plan price")
		return
	}
	if fetched.From != signer {
		writeSessionErr(w, http.StatusUnprocessableEntity, codeTransferFromMismatch, "transfer was sent from a different wallet than the signer")
		return
	}
	if fetched.Confirmations < a.minConfirmations {
		writeSessionErr(w, http.StatusTooEarly, codeTransferNotMined, "transfer below minimum confirmations")
		return
	}
	logFrom, logTo, logOK := chain.DecodeTransferLog(fetched.Receipt, a.tokenAddr, expectedPrice)
	if !logOK || logFrom != signer || logTo != a.treasuryAddr {
		writeSessionErr(w, http.StatusUnprocessableEntity, codeTransferLogMismatch, "Transfer log does not match calldata")
		return
	}

	// 4. Allowance check (authoritative). approval_tx_hash is informational.
	allowance, err := a.chain.Allowance(r.Context(), a.tokenAddr, signer, a.chain.Contract())
	if err != nil {
		writeSessionErr(w, http.StatusBadGateway, codeUpstreamError, "allowance read failed: "+err.Error())
		return
	}
	renewalCount := big.NewInt(int64(a.approvePeriodsHint - 1))
	if renewalCount.Sign() < 0 {
		renewalCount = big.NewInt(0)
	}
	required := new(big.Int).Mul(expectedPrice, renewalCount)
	if allowance.Cmp(required) < 0 {
		writeSessionErr(w, http.StatusUnprocessableEntity, codeInsufficientAllowance, "wallet allowance is below the renewal target")
		return
	}
	if strings.TrimSpace(req.ApprovalTxHash) != "" {
		if code, msg := a.verifyApprovalTx(r.Context(), req.ApprovalTxHash, signer); code != "" {
			writeSessionErr(w, http.StatusUnprocessableEntity, code, msg)
			return
		}
	}

	// 5+6. Commit (double-spend guard inside CompleteSession), then upsert
	//      subscription, then webhooks. SubscriptionID == address; this is
	//      stable and matches the existing GET /subscriptions/{address} lookup.
	subID := strings.ToLower(signer.Hex())
	approvalHash := strings.ToLower(strings.TrimSpace(req.ApprovalTxHash))
	completedAt := a.now()
	if err := a.store.CompleteSession(r.Context(), sess.ID, subID, transferHash, approvalHash, subID, completedAt); err != nil {
		switch {
		case errors.Is(err, store.ErrTxAlreadyConsumed):
			writeSessionErr(w, http.StatusConflict, codeTransferAlreadyConsumed, "this payment has already been used for a different session")
		case errors.Is(err, store.ErrSessionNotPending):
			writeSessionErr(w, http.StatusConflict, codeSessionCompleted, "session is no longer pending")
		case errors.Is(err, store.ErrSessionNotFound):
			writeSessionErr(w, http.StatusNotFound, codeSessionNotFound, "session not found")
		default:
			writeSessionErr(w, http.StatusInternalServerError, codeUpstreamError, err.Error())
		}
		return
	}

	// Subscription record. start_at = now + plan.period, because the user
	// already paid month 1 directly to the treasury — the scheduler waits
	// one full period before its first pull.
	existing, _ := a.store.GetSubscription(r.Context(), subID)
	nextCharge := plan.AddPeriod(completedAt)
	sub := &store.Subscription{
		User:          subID,
		PlanID:        plan.ID,
		Status:        store.StatusActive,
		NextAttemptAt: nextCharge,
		CreatedAt:     completedAt,
	}
	if existing != nil {
		// Replay: wipe past dunning + last-charge state but keep CreatedAt
		// for the user's history.
		sub.CreatedAt = existing.CreatedAt
	}
	if err := a.store.UpsertSubscription(r.Context(), sub); err != nil {
		log.Printf("sessions: upsert sub failed after CompleteSession: %v", err)
		// Don't fail the request — the session is already marked completed
		// and the subscription will be recoverable. Fall through to webhook.
	}

	a.emitSessionCompleted(r.Context(), sess, sub, plan, completedAt, false)
	if existing != nil {
		a.emitSubscriptionReplaced(r.Context(), sess, existing, sub)
	}

	writeJSON(w, http.StatusOK, completeSessionResp{
		SessionID:      sess.ID,
		Status:         store.SessionStatusCompleted,
		Wallet:         subID,
		SubscriptionID: subID,
		NextChargeAt:   nextCharge,
	})
}

func (a *API) verifyApprovalTx(ctx context.Context, hashHex string, signer common.Address) (code, msg string) {
	hashHex = normalizeHash(hashHex)
	if !isHashLike(hashHex) {
		return codeBadRequest, "approval_tx_hash is not a 0x-prefixed 32-byte hash"
	}
	fetched, err := a.chain.Fetch(ctx, common.HexToHash(hashHex))
	if err == chain.ErrTxNotMined {
		return codeTransferNotMined, "approval not yet mined"
	}
	if err != nil {
		return codeUpstreamError, err.Error()
	}
	if fetched.Receipt.Status != 1 {
		return codeApprovalReverted, "approval tx reverted on-chain"
	}
	if fetched.Tx.To() == nil || *fetched.Tx.To() != a.tokenAddr {
		return codeApprovalWrongToken, "approval is not against the configured token"
	}
	spender, _, decoded := chain.DecodeApproveCall(fetched.Tx.Data())
	if !decoded {
		return codeApprovalWrongMethod, "calldata is not ERC-20 approve()"
	}
	if spender != a.chain.Contract() {
		return codeApprovalWrongSpender, "approval spender is not the Subscriptions contract"
	}
	if fetched.From != signer {
		return codeApprovalFromMismatch, "approval was sent from a different wallet than the signer"
	}
	return "", ""
}

// ---------- POST /admin/sessions/{id}/force_complete ----------
//
// Operator backstop for "user paid month 1 then closed the tab and never
// finished the signature step." Runs the same on-chain verification as the
// public /complete handler, minus the EIP-191 signature check (admin auth
// stands in for it). The webhook is emitted with `recovered_by:"operator"`
// in `data` so the integrator can tell organic vs. recovered completions
// apart.
type forceCompleteReq struct {
	InitialTransferTx string `json:"initial_transfer_tx"`
}

func (a *API) forceCompleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req forceCompleteReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.InitialTransferTx == "" {
		writeSessionErr(w, http.StatusBadRequest, codeBadRequest, "initial_transfer_tx required")
		return
	}
	sess, err := a.store.GetSession(r.Context(), id)
	if err != nil {
		writeSessionErr(w, http.StatusInternalServerError, codeUpstreamError, err.Error())
		return
	}
	if sess == nil {
		writeSessionErr(w, http.StatusNotFound, codeSessionNotFound, "session not found")
		return
	}
	if sess.Status != store.SessionStatusPending {
		writeSessionErr(w, http.StatusConflict, codeSessionCompleted, "session no longer pending")
		return
	}
	plan, err := a.store.GetPlan(r.Context(), sess.PlanID)
	if err != nil || plan == nil {
		writeSessionErr(w, http.StatusInternalServerError, codeUpstreamError, "plan lookup failed")
		return
	}
	expectedPrice, _ := new(big.Int).SetString(plan.PriceAtomic, 10)

	transferHash := normalizeHash(req.InitialTransferTx)
	fetched, err := a.chain.Fetch(r.Context(), common.HexToHash(transferHash))
	if err == chain.ErrTxNotMined {
		writeSessionErr(w, http.StatusTooEarly, codeTransferNotMined, "transfer not yet mined")
		return
	}
	if err != nil {
		writeSessionErr(w, http.StatusBadGateway, codeUpstreamError, err.Error())
		return
	}
	if fetched.Receipt.Status != 1 {
		writeSessionErr(w, http.StatusUnprocessableEntity, codeTransferReverted, "transfer tx reverted on-chain")
		return
	}
	if fetched.Tx.To() == nil || *fetched.Tx.To() != a.tokenAddr {
		writeSessionErr(w, http.StatusUnprocessableEntity, codeTransferWrongToken, "transfer not against configured token")
		return
	}
	to, value, ok := chain.DecodeTransferCall(fetched.Tx.Data())
	if !ok || to != a.treasuryAddr || value.Cmp(expectedPrice) != 0 {
		writeSessionErr(w, http.StatusUnprocessableEntity, codeTransferWrongAmount, "transfer did not match plan price + treasury")
		return
	}
	signer := fetched.From
	subID := strings.ToLower(signer.Hex())
	completedAt := a.now()

	if err := a.store.CompleteSession(r.Context(), sess.ID, subID, transferHash, "", subID, completedAt); err != nil {
		switch {
		case errors.Is(err, store.ErrTxAlreadyConsumed):
			writeSessionErr(w, http.StatusConflict, codeTransferAlreadyConsumed, "this payment is already attached to another session")
		default:
			writeSessionErr(w, http.StatusInternalServerError, codeUpstreamError, err.Error())
		}
		return
	}
	existing, _ := a.store.GetSubscription(r.Context(), subID)
	nextCharge := plan.AddPeriod(completedAt)
	sub := &store.Subscription{
		User: subID, PlanID: plan.ID, Status: store.StatusActive,
		NextAttemptAt: nextCharge, CreatedAt: completedAt,
	}
	if existing != nil {
		sub.CreatedAt = existing.CreatedAt
	}
	_ = a.store.UpsertSubscription(r.Context(), sub)
	a.emitSessionCompleted(r.Context(), sess, sub, plan, completedAt, true)
	if existing != nil {
		a.emitSubscriptionReplaced(r.Context(), sess, existing, sub)
	}

	writeJSON(w, http.StatusOK, completeSessionResp{
		SessionID:      sess.ID,
		Status:         store.SessionStatusCompleted,
		Wallet:         subID,
		SubscriptionID: subID,
		NextChargeAt:   nextCharge,
	})
}

// ---------- webhooks ----------

func (a *API) emitSessionCompleted(ctx context.Context, sess *store.CheckoutSession, sub *store.Subscription, plan *store.Plan, at time.Time, recovered bool) {
	if a.webhook == nil {
		return
	}
	data := map[string]any{
		"session_id":           sess.ID,
		"plan_id":              plan.ID,
		"wallet":               sub.User,
		"subscription_id":      sub.User,
		"initial_transfer_tx":  sess.InitialTransferTx,
		"approval_tx_hash":     sess.ApprovalTxHash,
		"next_charge_at":       sub.NextAttemptAt,
		"metadata":             sess.Metadata,
	}
	if recovered {
		data["recovered_by"] = "operator"
	}
	go func() {
		if err := a.webhook.Send(ctx, webhooks.EventSessionCompleted, data); err != nil {
			log.Printf("webhook %s: %v", webhooks.EventSessionCompleted, err)
		}
	}()
}

func (a *API) emitSubscriptionReplaced(ctx context.Context, sess *store.CheckoutSession, old, new *store.Subscription) {
	if a.webhook == nil {
		return
	}
	data := map[string]any{
		"wallet":                old.User,
		"old_subscription_id":   old.User,
		"new_subscription_id":   new.User,
		"old_plan_id":           old.PlanID,
		"new_plan_id":           new.PlanID,
		"session_id":            sess.ID,
		"metadata":              sess.Metadata,
	}
	go func() {
		if err := a.webhook.Send(ctx, webhooks.EventSubscriptionReplaced, data); err != nil {
			log.Printf("webhook %s: %v", webhooks.EventSubscriptionReplaced, err)
		}
	}()
}

// ---------- sweeper ----------

// RunSessionSweeper expires any pending session past its TTL. Returns when
// ctx is done. Intended to be started as a goroutine from cmd/server.
func (a *API) RunSessionSweeper(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := a.store.ExpireSessionsBefore(ctx, a.now())
			if err != nil {
				log.Printf("session sweep: %v", err)
				continue
			}
			if n > 0 {
				log.Printf("session sweep: expired %d session(s)", n)
			}
		}
	}
}

// ---------- helpers ----------

// writeSessionErr writes the typed error envelope the checkout page parses.
func writeSessionErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

func newSessionID() string {
	var buf [10]byte
	_, _ = rand.Read(buf[:])
	return "cs_" + hex.EncodeToString(buf[:])
}

func normalizeHash(h string) string {
	h = strings.TrimSpace(strings.ToLower(h))
	if !strings.HasPrefix(h, "0x") {
		h = "0x" + h
	}
	return h
}

// isHashLike checks 0x-prefixed 32-byte hex.
func isHashLike(h string) bool {
	if len(h) != 66 || !strings.HasPrefix(h, "0x") {
		return false
	}
	_, err := ethhexutil.Decode(h)
	return err == nil
}
