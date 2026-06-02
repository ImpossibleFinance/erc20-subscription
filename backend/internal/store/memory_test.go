package store

import (
	"context"
	"testing"
	"time"
)

func TestMemory_PlanRoundTrip(t *testing.T) {
	m := NewMemory()
	p := &Plan{ID: "pro", PriceAtomic: "10000000", PeriodCount: 1, PeriodUnit: PeriodUnitMonth, Active: true}
	if err := m.UpsertPlan(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	got, _ := m.GetPlan(context.Background(), "pro")
	if got == nil || got.PriceAtomic != "10000000" {
		t.Errorf("got %+v", got)
	}
}

func TestMemory_DueBefore_OnlyBillableStatus(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0).UTC()

	due := &Subscription{User: "0xA", Status: StatusActive, NextAttemptAt: now.Add(-time.Hour)}
	notDue := &Subscription{User: "0xB", Status: StatusActive, NextAttemptAt: now.Add(time.Hour)}
	cancelled := &Subscription{User: "0xC", Status: StatusCancelled, NextAttemptAt: now.Add(-time.Hour)}
	pastDue := &Subscription{User: "0xD", Status: StatusPastDue, NextAttemptAt: now.Add(-time.Hour)}
	for _, s := range []*Subscription{due, notDue, cancelled, pastDue} {
		_ = m.UpsertSubscription(ctx, s)
	}
	got, err := m.DueBefore(ctx, now, 100)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"0xa": true, "0xd": true}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2 (active+past_due that are due)", len(got))
	}
	for _, s := range got {
		if !want[s.User] {
			t.Errorf("unexpected sub %s", s.User)
		}
	}
}

func TestMemory_DueBefore_LimitAndOrder(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0).UTC()
	for i, ts := range []int64{-300, -200, -100} {
		_ = m.UpsertSubscription(ctx, &Subscription{
			User:          string(rune('A' + i)),
			Status:        StatusActive,
			NextAttemptAt: now.Add(time.Duration(ts) * time.Second),
		})
	}
	got, _ := m.DueBefore(ctx, now, 2)
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
	// oldest first
	if !got[0].NextAttemptAt.Before(got[1].NextAttemptAt) {
		t.Error("expected ascending order by NextAttemptAt")
	}
}
