package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeLocker is an in-memory Locker that mirrors the contract for RedisLocker:
// exclusive Acquire (returns false if held), Renew only succeeds if holder
// matches, Release only succeeds if holder matches. TTL is honored against
// the fake clock so we can simulate expiry.
type fakeLocker struct {
	mu        sync.Mutex
	now       time.Time
	holder    string
	expiresAt time.Time
}

func newFakeLocker(now time.Time) *fakeLocker { return &fakeLocker{now: now} }

func (f *fakeLocker) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func (f *fakeLocker) Acquire(_ context.Context, _, holder string, ttl time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.holder != "" && f.now.Before(f.expiresAt) {
		return false, nil
	}
	f.holder = holder
	f.expiresAt = f.now.Add(ttl)
	return true, nil
}

func (f *fakeLocker) Renew(_ context.Context, _, holder string, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.holder != holder || !f.now.Before(f.expiresAt) {
		return errors.New("lost lock")
	}
	f.expiresAt = f.now.Add(ttl)
	return nil
}

func (f *fakeLocker) Release(_ context.Context, _, holder string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.holder == holder {
		f.holder = ""
		f.expiresAt = time.Time{}
	}
	return nil
}

func TestLock_FirstAcquireSucceeds(t *testing.T) {
	l := newFakeLocker(time.Now())
	ok, err := l.Acquire(context.Background(), "k", "a", time.Second)
	if err != nil || !ok {
		t.Errorf("Acquire=%v,%v want true,nil", ok, err)
	}
}

func TestLock_SecondAcquireBlocks(t *testing.T) {
	l := newFakeLocker(time.Now())
	_, _ = l.Acquire(context.Background(), "k", "a", time.Second)
	ok, err := l.Acquire(context.Background(), "k", "b", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("second Acquire should fail while held")
	}
}

func TestLock_ExpiryAllowsTakeover(t *testing.T) {
	l := newFakeLocker(time.Now())
	_, _ = l.Acquire(context.Background(), "k", "a", time.Second)
	l.advance(2 * time.Second) // TTL expired
	ok, err := l.Acquire(context.Background(), "k", "b", time.Second)
	if err != nil || !ok {
		t.Errorf("after expiry Acquire by b should succeed, got ok=%v err=%v", ok, err)
	}
}

func TestLock_RenewByHolder(t *testing.T) {
	l := newFakeLocker(time.Now())
	_, _ = l.Acquire(context.Background(), "k", "a", time.Second)
	if err := l.Renew(context.Background(), "k", "a", 2*time.Second); err != nil {
		t.Errorf("Renew by holder should succeed, got %v", err)
	}
}

func TestLock_RenewByOtherFails(t *testing.T) {
	l := newFakeLocker(time.Now())
	_, _ = l.Acquire(context.Background(), "k", "a", time.Second)
	if err := l.Renew(context.Background(), "k", "b", time.Second); err == nil {
		t.Error("Renew by non-holder should fail")
	}
}

func TestLock_ReleaseByOtherIsNoOp(t *testing.T) {
	l := newFakeLocker(time.Now())
	_, _ = l.Acquire(context.Background(), "k", "a", time.Second)
	_ = l.Release(context.Background(), "k", "b")
	// "a" should still hold it.
	ok, _ := l.Acquire(context.Background(), "k", "c", time.Second)
	if ok {
		t.Error("Acquire by c should fail; a still holds")
	}
}

func TestNoOpLocker_AlwaysAcquires(t *testing.T) {
	l := NoOpLocker{}
	for i := 0; i < 5; i++ {
		ok, err := l.Acquire(context.Background(), "k", "h", time.Second)
		if err != nil || !ok {
			t.Fatalf("NoOpLocker.Acquire failed: ok=%v err=%v", ok, err)
		}
	}
}

func TestInstanceID_Unique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := generateInstanceID()
		if seen[id] {
			t.Errorf("duplicate instance id: %s", id)
		}
		seen[id] = true
	}
}
