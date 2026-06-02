package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/impossiblefinance/erc20-subscription/backend/internal/dunning"
	"github.com/impossiblefinance/erc20-subscription/backend/internal/store"
)

func newSub(user, plan string) *store.Subscription {
	return &store.Subscription{
		User:          strings.ToLower(user),
		PlanID:        plan,
		Status:        store.StatusActive,
		NextAttemptAt: time.Unix(1_700_000_000, 0).UTC(),
	}
}

func newPlan(id string, count uint32, unit string) *store.Plan {
	return &store.Plan{ID: id, PriceAtomic: "10000000", PeriodCount: count, PeriodUnit: unit, Active: true}
}

func TestHandleFailure_FirstAttemptMarksPastDue(t *testing.T) {
	mem := store.NewMemory()
	s := &Scheduler{
		store:  mem,
		policy: dunning.Policy{Backoffs: []time.Duration{24 * time.Hour, 72 * time.Hour}},
		now:    func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	}
	sub := newSub("0xAAA1", "pro")
	plan := newPlan("pro", 30, store.PeriodUnitDay)
	_ = mem.UpsertPlan(context.Background(), plan)
	_ = mem.UpsertSubscription(context.Background(), sub)

	if err := s.handleFailure(context.Background(), sub, plan, errors.New("insufficient balance")); err != nil {
		t.Fatalf("handleFailure: %v", err)
	}
	got, _ := mem.GetSubscription(context.Background(), sub.User)
	if got.Status != store.StatusPastDue {
		t.Errorf("status=%s want past_due", got.Status)
	}
	if got.DunningAttempts != 1 {
		t.Errorf("attempts=%d want 1", got.DunningAttempts)
	}
	want := s.now().Add(24 * time.Hour)
	if !got.NextAttemptAt.Equal(want) {
		t.Errorf("nextAttemptAt=%v want %v", got.NextAttemptAt, want)
	}
}

func TestHandleFailure_SecondAttemptUsesSecondBackoff(t *testing.T) {
	mem := store.NewMemory()
	s := &Scheduler{
		store:  mem,
		policy: dunning.Policy{Backoffs: []time.Duration{24 * time.Hour, 72 * time.Hour, 168 * time.Hour}},
		now:    func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	}
	sub := newSub("0xAAA3", "pro")
	sub.DunningAttempts = 1
	sub.Status = store.StatusPastDue
	plan := newPlan("pro", 30, store.PeriodUnitDay)
	_ = mem.UpsertPlan(context.Background(), plan)
	_ = mem.UpsertSubscription(context.Background(), sub)

	if err := s.handleFailure(context.Background(), sub, plan, errors.New("again")); err != nil {
		t.Fatalf("handleFailure: %v", err)
	}
	got, _ := mem.GetSubscription(context.Background(), sub.User)
	if got.DunningAttempts != 2 {
		t.Errorf("attempts=%d want 2", got.DunningAttempts)
	}
	want := s.now().Add(72 * time.Hour)
	if !got.NextAttemptAt.Equal(want) {
		t.Errorf("nextAttemptAt=%v want %v", got.NextAttemptAt, want)
	}
}

func TestHandleFailure_ExhaustedMarksCancelled(t *testing.T) {
	mem := store.NewMemory()
	s := &Scheduler{
		store:  mem,
		policy: dunning.Policy{Backoffs: []time.Duration{24 * time.Hour}}, // single attempt
		now:    func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	}
	sub := newSub("0xAAA2", "pro")
	sub.DunningAttempts = 1
	plan := newPlan("pro", 30, store.PeriodUnitDay)
	_ = mem.UpsertPlan(context.Background(), plan)
	_ = mem.UpsertSubscription(context.Background(), sub)

	if err := s.handleFailure(context.Background(), sub, plan, errors.New("final")); err != nil {
		t.Fatalf("handleFailure: %v", err)
	}
	got, _ := mem.GetSubscription(context.Background(), sub.User)
	if got.Status != store.StatusCancelled {
		t.Errorf("status=%s want cancelled", got.Status)
	}
}

func TestMarkCharged_AdvancesByPeriodNotWallClock(t *testing.T) {
	mem := store.NewMemory()
	scheduled := time.Unix(1_700_000_000, 0).UTC()
	s := &Scheduler{
		store: mem,
		// "Now" is 5 days late — verify NextAttemptAt advances from scheduled, not now.
		now: func() time.Time { return scheduled.Add(5 * 24 * time.Hour) },
	}
	sub := newSub("0xC0FFEE", "pro")
	sub.NextAttemptAt = scheduled
	plan := newPlan("pro", 30, store.PeriodUnitDay)
	_ = mem.UpsertPlan(context.Background(), plan)
	_ = mem.UpsertSubscription(context.Background(), sub)

	if err := s.markCharged(context.Background(), sub, plan, common.HexToHash("0xdead"), 12345); err != nil {
		t.Fatalf("markCharged: %v", err)
	}
	got, _ := mem.GetSubscription(context.Background(), sub.User)
	want := plan.AddPeriod(scheduled)
	if !got.NextAttemptAt.Equal(want) {
		t.Errorf("nextAttemptAt=%v want %v (period from scheduled, not now)", got.NextAttemptAt, want)
	}
	if got.DunningAttempts != 0 {
		t.Errorf("attempts=%d want reset to 0", got.DunningAttempts)
	}
	if got.LastChargedTx == "" {
		t.Error("LastChargedTx should be set")
	}
	if got.InFlightTx != "" {
		t.Errorf("InFlightTx=%q want cleared", got.InFlightTx)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello world", 5); got != "hello…" {
		t.Errorf("got %q", got)
	}
	if got := truncate("short", 100); got != "short" {
		t.Errorf("got %q", got)
	}
}
