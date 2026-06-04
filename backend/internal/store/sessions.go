package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// CheckoutSession is a hosted-checkout session minted by an integrator. It
// holds intent + post-completion state. See docs/checkout-sessions.md for the
// full design.
//
// Status transitions: pending → completed (terminal) | pending → expired
// (terminal). There is no "failed" state — verification failures return an
// error and leave the session in pending so the user can retry.
type CheckoutSession struct {
	ID                string          `json:"id"`
	PlanID            string          `json:"plan_id"`
	Status            string          `json:"status"` // pending | completed | expired
	Wallet            string          `json:"wallet,omitempty"`             // lowercase 0x…; "" until completed
	InitialTransferTx string          `json:"initial_transfer_tx,omitempty"` // lowercase 0x hash; "" until completed
	ApprovalTxHash    string          `json:"approval_tx_hash,omitempty"`
	SubscriptionID    string          `json:"subscription_id,omitempty"` // populated on completion
	SuccessURL        string          `json:"success_url,omitempty"`
	Metadata          json.RawMessage `json:"metadata,omitempty"`
	ExpiresAt         time.Time       `json:"expires_at"`
	CompletedAt       *time.Time      `json:"completed_at,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
}

const (
	SessionStatusPending   = "pending"
	SessionStatusCompleted = "completed"
	SessionStatusExpired   = "expired"
)

// Errors the SessionStore returns from Complete. Mapped to typed API error
// codes by the handler (see internal/api/sessions.go).
var (
	ErrSessionNotFound   = errors.New("session not found")
	ErrSessionNotPending = errors.New("session is not pending")
	ErrTxAlreadyConsumed = errors.New("transfer tx already consumed by another session")
)

// SessionStore is the subset of Store used by the hosted-checkout flow. Both
// MemoryStore and RedisStore implement it.
//
// Complete is a CAS: it must atomically (a) check the session is pending and
// not past TTL, (b) check no OTHER session has already claimed this
// `transferTx`, (c) mark the session completed with the recovered wallet +
// tx hashes + subscription id. Implementations serialize concurrent
// completions on the same session id and the same tx hash.
type SessionStore interface {
	CreateSession(ctx context.Context, s *CheckoutSession) error
	GetSession(ctx context.Context, id string) (*CheckoutSession, error)
	CompleteSession(ctx context.Context, id, wallet, transferTx, approvalTx, subscriptionID string, at time.Time) error
	// ExpireSessionsBefore flips any pending sessions whose ExpiresAt is at
	// or before `t` to expired. Returns how many were expired.
	ExpireSessionsBefore(ctx context.Context, t time.Time) (int, error)
}
