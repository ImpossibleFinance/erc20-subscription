package scheduler

import (
	"context"
	"time"
)

// Locker serializes tick execution across multiple scheduler instances. The
// operator EOA has a single nonce sequence, so at most one process can be
// signing pull() txs at a time. Tick acquires this lock at the top, runs
// under heartbeat, and releases at the end.
//
// Implementations must guarantee:
//   - Acquire: atomic SETNX semantics with TTL.
//   - Renew: only extends TTL if `holderID` still matches (so a dead
//     instance can't keep extending after a takeover).
//   - Release: only deletes if `holderID` still matches.
//
// A holder that dies without releasing must lose the lock at most `ttl` later.
type Locker interface {
	Acquire(ctx context.Context, key, holderID string, ttl time.Duration) (bool, error)
	Renew(ctx context.Context, key, holderID string, ttl time.Duration) error
	Release(ctx context.Context, key, holderID string) error
}

// NoOpLocker is the always-acquires implementation for tests and single-
// instance dev runs. NEVER use this in a multi-replica deployment — it gives
// no mutual exclusion guarantees.
type NoOpLocker struct{}

func (NoOpLocker) Acquire(_ context.Context, _, _ string, _ time.Duration) (bool, error) {
	return true, nil
}
func (NoOpLocker) Renew(_ context.Context, _, _ string, _ time.Duration) error { return nil }
func (NoOpLocker) Release(_ context.Context, _, _ string) error                { return nil }
