package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestMemorySessionRoundTrip(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	sess := &CheckoutSession{
		ID:        "cs_test",
		PlanID:    "pro_monthly_usdc",
		Status:    SessionStatusPending,
		Metadata:  json.RawMessage(`{"user_id":"u_1"}`),
		CreatedAt: now,
		ExpiresAt: now.Add(15 * time.Minute),
	}
	if err := m.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	got, err := m.GetSession(ctx, "cs_test")
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if got.PlanID != sess.PlanID || got.Status != SessionStatusPending {
		t.Fatalf("mismatch: %+v", got)
	}
}

func TestMemoryCompleteSessionHappyPath(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	now := time.Now().UTC()
	_ = m.CreateSession(ctx, &CheckoutSession{
		ID: "cs_1", PlanID: "pro", Status: SessionStatusPending,
		CreatedAt: now, ExpiresAt: now.Add(15 * time.Minute),
	})
	if err := m.CompleteSession(ctx, "cs_1", "0xWALLET", "0xTX1", "0xAPPR", "sub_x", now); err != nil {
		t.Fatalf("complete: %v", err)
	}
	got, _ := m.GetSession(ctx, "cs_1")
	if got.Status != SessionStatusCompleted {
		t.Fatalf("status: %s", got.Status)
	}
	if got.Wallet != "0xwallet" {
		t.Fatalf("wallet not lowercased: %q", got.Wallet)
	}
	if got.InitialTransferTx != "0xtx1" {
		t.Fatalf("tx: %q", got.InitialTransferTx)
	}
	if got.SubscriptionID != "sub_x" {
		t.Fatalf("subscription_id: %q", got.SubscriptionID)
	}
	if got.CompletedAt == nil || !got.CompletedAt.Equal(now) {
		t.Fatalf("completed_at: %v", got.CompletedAt)
	}
}

func TestMemoryCompleteRejectsDoubleSpend(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	now := time.Now().UTC()
	for _, id := range []string{"cs_a", "cs_b"} {
		_ = m.CreateSession(ctx, &CheckoutSession{
			ID: id, PlanID: "p", Status: SessionStatusPending,
			CreatedAt: now, ExpiresAt: now.Add(15 * time.Minute),
		})
	}
	if err := m.CompleteSession(ctx, "cs_a", "0xA", "0xSAME", "", "sub_a", now); err != nil {
		t.Fatalf("first complete: %v", err)
	}
	err := m.CompleteSession(ctx, "cs_b", "0xB", "0xSAME", "", "sub_b", now)
	if !errors.Is(err, ErrTxAlreadyConsumed) {
		t.Fatalf("expected ErrTxAlreadyConsumed, got %v", err)
	}
}

func TestMemoryCompleteIdempotentSameTx(t *testing.T) {
	// Re-running CompleteSession on an already-completed session with the
	// same tx hash must return ErrSessionNotPending (the handler maps that
	// to an idempotent 200 by reading the prior completion result first).
	ctx := context.Background()
	m := NewMemory()
	now := time.Now().UTC()
	_ = m.CreateSession(ctx, &CheckoutSession{
		ID: "cs_x", PlanID: "p", Status: SessionStatusPending,
		CreatedAt: now, ExpiresAt: now.Add(15 * time.Minute),
	})
	_ = m.CompleteSession(ctx, "cs_x", "0xW", "0xT", "", "sub_x", now)
	err := m.CompleteSession(ctx, "cs_x", "0xW", "0xT", "", "sub_x", now)
	if !errors.Is(err, ErrSessionNotPending) {
		t.Fatalf("expected ErrSessionNotPending, got %v", err)
	}
}

func TestMemoryCompleteUnknownSession(t *testing.T) {
	m := NewMemory()
	err := m.CompleteSession(context.Background(), "cs_nope", "0xW", "0xT", "", "sub_x", time.Now())
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound, got %v", err)
	}
}

func TestMemoryExpireSessionsBefore(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	now := time.Now().UTC()
	_ = m.CreateSession(ctx, &CheckoutSession{
		ID: "cs_expired", PlanID: "p", Status: SessionStatusPending,
		CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Minute),
	})
	_ = m.CreateSession(ctx, &CheckoutSession{
		ID: "cs_fresh", PlanID: "p", Status: SessionStatusPending,
		CreatedAt: now, ExpiresAt: now.Add(15 * time.Minute),
	})
	n, err := m.ExpireSessionsBefore(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 expired, got %d", n)
	}
	got, _ := m.GetSession(ctx, "cs_expired")
	if got.Status != SessionStatusExpired {
		t.Fatalf("status: %s", got.Status)
	}
	fresh, _ := m.GetSession(ctx, "cs_fresh")
	if fresh.Status != SessionStatusPending {
		t.Fatalf("fresh session should stay pending, got %s", fresh.Status)
	}
}
