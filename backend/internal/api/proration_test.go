package api

import (
	"math/big"
	"testing"
	"time"

	"github.com/impossiblefinance/erc20-subscription/backend/internal/store"
)

func mkPlan(id string, price string) *store.Plan {
	return &store.Plan{ID: id, PriceAtomic: price, PeriodCount: 1, PeriodUnit: store.PeriodUnitMonth, Active: true}
}

func TestProratedDiff_UpgradeMidCycle(t *testing.T) {
	// Cycle: Jan 1 → Feb 1 (31 days). Upgrading on Jan 15 with 17 days left.
	// Pro $10 → Team $50. Diff = (50 - 10) × 17/31 ≈ $21.94 → 21935483 atomic.
	sub := &store.Subscription{
		LastChargedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		NextAttemptAt: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
	}
	now := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	old := mkPlan("pro", "10000000")
	new_ := mkPlan("team", "50000000")
	got := proratedDiff(now, sub, old, new_)
	if got == nil {
		t.Fatal("got nil, want diff")
	}
	// Expected: 40_000_000 × 17 / 31 = 21_935_483 (integer truncation).
	want := big.NewInt(21_935_483)
	if got.Cmp(want) != 0 {
		t.Errorf("got %s want %s", got, want)
	}
}

func TestProratedDiff_DowngradeReturnsNil(t *testing.T) {
	sub := &store.Subscription{
		LastChargedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		NextAttemptAt: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
	}
	now := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	old := mkPlan("team", "50000000")
	new_ := mkPlan("pro", "10000000")
	if got := proratedDiff(now, sub, old, new_); got != nil {
		t.Errorf("downgrade got %s, want nil (no refunds)", got)
	}
}

func TestProratedDiff_NoLastChargedAt(t *testing.T) {
	// Brand-new sub that's never been charged; can't prorate.
	sub := &store.Subscription{
		NextAttemptAt: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
	}
	now := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	if got := proratedDiff(now, sub, mkPlan("pro", "10000000"), mkPlan("team", "50000000")); got != nil {
		t.Errorf("got %s, want nil (no history)", got)
	}
}

func TestProratedDiff_CycleAlreadyOver(t *testing.T) {
	// Past the cycle end — no proration; the next cycle will naturally
	// charge at the new price.
	sub := &store.Subscription{
		LastChargedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		NextAttemptAt: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
	}
	now := time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC)
	if got := proratedDiff(now, sub, mkPlan("pro", "10000000"), mkPlan("team", "50000000")); got != nil {
		t.Errorf("got %s, want nil (cycle ended)", got)
	}
}
