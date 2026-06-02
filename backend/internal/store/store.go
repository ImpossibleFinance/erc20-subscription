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
	ID          string // human id like "pro_monthly"
	PriceAtomic string // token smallest unit, base-10 string (USDC has 6 decimals)
	PeriodCount uint32
	PeriodUnit  string // "day" | "month" | "year"
	Active      bool
	CreatedAt   time.Time
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
	User             string    // 0x address, lowercased
	PlanID           string
	Status           string    // active | past_due | cancelled
	NextAttemptAt    time.Time // when the scheduler should next attempt a pull
	DunningAttempts  int       // failed attempts in current cycle (0 when healthy)
	LastError        string
	LastChargedTx    string    // tx hash of most recent successful pull
	LastChargedBlock uint64
	// InFlightTx is the hash of a submitted pull tx whose receipt we never
	// confirmed (RPC timeout / context cancel). On next tick, the scheduler
	// MUST poll this receipt before submitting another pull, to avoid
	// double-charging if the original tx eventually mined.
	InFlightTx string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Store is the persistence interface. Implementations must be safe for
// concurrent use.
type Store interface {
	UpsertPlan(ctx context.Context, p *Plan) error
	GetPlan(ctx context.Context, id string) (*Plan, error)
	ListPlans(ctx context.Context) ([]*Plan, error)

	UpsertSubscription(ctx context.Context, s *Subscription) error
	GetSubscription(ctx context.Context, user string) (*Subscription, error)
	ListSubscriptions(ctx context.Context, status string) ([]*Subscription, error)

	// DueBefore returns active or past_due subscriptions whose NextAttemptAt
	// is at or before `t`, up to `limit`.
	DueBefore(ctx context.Context, t time.Time, limit int) ([]*Subscription, error)
}
