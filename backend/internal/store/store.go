// Package store persists plans, subscriptions, and dunning state.
//
// Subscriptions are indexed by lowercased 0x-prefixed wallet address. The
// scheduler reads `DueBefore` to find subs whose next-attempt time has
// passed; on success/failure the scheduler updates that sub.
package store

import (
	"context"
	"time"
)

// Plan is purely off-chain — the contract has no concept of plans. We store
// what to charge (PriceAtomic) and how often (PeriodCount × PeriodUnit).
//
// PeriodUnit must be "day", "month", or "year". Months and years use calendar
// arithmetic with end-of-month clamping (Jan 31 → Feb 28 → Mar 31 → ...) so
// that an integrator who picks "1 month" doesn't drift relative to the
// calendar over a year.
type Plan struct {
	ID          string    `json:"id"`           // human id like "pro_monthly"
	PriceAtomic string    `json:"price_atomic"` // token smallest unit, base-10 string (USDC has 6 decimals)
	PeriodCount uint32    `json:"period_count"`
	PeriodUnit  string    `json:"period_unit"` // "day" | "month" | "year"
	Active      bool      `json:"active"`
	CreatedAt   time.Time `json:"created_at"`
}

// ValidPeriodUnit reports whether u is one of the supported unit values.
func ValidPeriodUnit(u string) bool {
	return u == PeriodUnitDay || u == PeriodUnitMonth || u == PeriodUnitYear
}

const (
	PeriodUnitDay   = "day"
	PeriodUnitMonth = "month"
	PeriodUnitYear  = "year"
)

// AddPeriod returns t advanced by the plan's period. For "month" and "year"
// it uses calendar arithmetic with end-of-month clamping: subscribing on
// Jan 31 produces next charges at Feb 28/29, Mar 31, Apr 30, … — the day
// never advances PAST the last day of the target month.
func (p *Plan) AddPeriod(t time.Time) time.Time {
	switch p.PeriodUnit {
	case PeriodUnitMonth:
		return clampedAddMonths(t, int(p.PeriodCount))
	case PeriodUnitYear:
		return clampedAddMonths(t, int(p.PeriodCount)*12)
	default: // PeriodUnitDay
		return t.AddDate(0, 0, int(p.PeriodCount))
	}
}

// clampedAddMonths adds n calendar months to t and clamps the resulting day
// to the last day of the target month. Go's time.AddDate(0,1,0) on Jan 31
// rolls forward to Mar 3 — wrong for billing. We want Feb 28.
func clampedAddMonths(t time.Time, n int) time.Time {
	y, m, d := t.Date()
	h, mi, s := t.Clock()
	target := time.Date(y, m+time.Month(n), 1, h, mi, s, 0, t.Location())
	// Last day of the target month = day 0 of the next month.
	lastDay := time.Date(target.Year(), target.Month()+1, 0, 0, 0, 0, 0, t.Location()).Day()
	if d > lastDay {
		d = lastDay
	}
	return time.Date(target.Year(), target.Month(), d, h, mi, s, 0, t.Location())
}

const (
	StatusActive    = "active"
	StatusPastDue   = "past_due"
	StatusCancelled = "cancelled"
)

// Subscription is the backend's record of a recurring charge against a wallet.
type Subscription struct {
	User   string `json:"user"`    // 0x address, lowercased
	PlanID string `json:"plan_id"`
	Status string `json:"status"` // active | past_due | cancelled

	// NextAttemptAt drives the due-queue: the scheduler picks subs whose
	// NextAttemptAt has passed. For a healthy active sub this is the next
	// regular billing date.
	NextAttemptAt time.Time `json:"next_attempt_at"`

	// LastChargedAt is when the most recent successful pull happened. Used
	// to compute the fraction of cycle remaining for proration math.
	LastChargedAt time.Time `json:"last_charged_at,omitempty"`

	// PendingChargeAtomic, when non-empty and > 0, is a one-time amount the
	// scheduler must pull BEFORE the next regular cycle charge. Set by the
	// API when a user upgrades plans mid-cycle (proration diff). Cleared on
	// successful pull.
	PendingChargeAtomic string `json:"pending_charge_atomic,omitempty"`

	DunningAttempts  int    `json:"dunning_attempts"`
	LastError        string `json:"last_error,omitempty"`
	LastChargedTx    string `json:"last_charged_tx,omitempty"`
	LastChargedBlock uint64 `json:"last_charged_block,omitempty"`

	// InFlightTx is the hash of a submitted pull tx whose receipt we never
	// confirmed (RPC timeout / context cancel). On next tick, the scheduler
	// MUST poll this receipt before submitting another pull, to avoid
	// double-charging if the original tx eventually mined.
	InFlightTx string `json:"in_flight_tx,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Store is the persistence interface. Implementations must be safe for
// concurrent use.
type Store interface {
	// SessionStore — hosted-checkout sessions; see sessions.go.
	SessionStore

	UpsertPlan(ctx context.Context, p *Plan) error
	GetPlan(ctx context.Context, id string) (*Plan, error)
	ListPlans(ctx context.Context) ([]*Plan, error)

	UpsertSubscription(ctx context.Context, s *Subscription) error
	GetSubscription(ctx context.Context, user string) (*Subscription, error)
	ListSubscriptions(ctx context.Context, status string) ([]*Subscription, error)

	// DueBefore returns active or past_due subscriptions whose NextAttemptAt
	// is at or before `t`, OR which have a PendingChargeAtomic to settle,
	// up to `limit`. Pending charges are always considered due so they get
	// processed as soon as allowance allows.
	DueBefore(ctx context.Context, t time.Time, limit int) ([]*Subscription, error)
}
