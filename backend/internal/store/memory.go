package store

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemoryStore is an in-memory Store for tests and local dev. Not durable.
type MemoryStore struct {
	mu       sync.RWMutex
	plans    map[string]*Plan
	subs     map[string]*Subscription   // key = lowercased address
	sessions map[string]*CheckoutSession
	txOwners map[string]string // key = lowercased tx hash, value = session id (reservation)
}

func NewMemory() *MemoryStore {
	return &MemoryStore{
		plans:    make(map[string]*Plan),
		subs:     make(map[string]*Subscription),
		sessions: make(map[string]*CheckoutSession),
		txOwners: make(map[string]string),
	}
}

func (m *MemoryStore) UpsertPlan(_ context.Context, p *Plan) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *p
	m.plans[p.ID] = &cp
	return nil
}

func (m *MemoryStore) GetPlan(_ context.Context, id string) (*Plan, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if p, ok := m.plans[id]; ok {
		cp := *p
		return &cp, nil
	}
	return nil, nil
}

func (m *MemoryStore) ListPlans(_ context.Context) ([]*Plan, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Plan, 0, len(m.plans))
	for _, p := range m.plans {
		cp := *p
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemoryStore) UpsertSubscription(_ context.Context, s *Subscription) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *s
	cp.User = strings.ToLower(cp.User)
	m.subs[cp.User] = &cp
	return nil
}

func (m *MemoryStore) GetSubscription(_ context.Context, user string) (*Subscription, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if s, ok := m.subs[strings.ToLower(user)]; ok {
		cp := *s
		return &cp, nil
	}
	return nil, nil
}

func (m *MemoryStore) ListSubscriptions(_ context.Context, status string) ([]*Subscription, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Subscription, 0, len(m.subs))
	for _, s := range m.subs {
		if status != "" && s.Status != status {
			continue
		}
		cp := *s
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].User < out[j].User })
	return out, nil
}

// ---------- sessions ----------

func (m *MemoryStore) CreateSession(_ context.Context, s *CheckoutSession) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *s
	m.sessions[s.ID] = &cp
	return nil
}

func (m *MemoryStore) GetSession(_ context.Context, id string) (*CheckoutSession, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if s, ok := m.sessions[id]; ok {
		cp := *s
		return &cp, nil
	}
	return nil, nil
}

func (m *MemoryStore) CompleteSession(_ context.Context, id, wallet, transferTx, approvalTx, subscriptionID string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[id]
	if !ok {
		return ErrSessionNotFound
	}
	if sess.Status != SessionStatusPending {
		return ErrSessionNotPending
	}
	hash := strings.ToLower(transferTx)
	if owner, taken := m.txOwners[hash]; taken && owner != id {
		return ErrTxAlreadyConsumed
	}
	completed := at
	sess.Status = SessionStatusCompleted
	sess.Wallet = strings.ToLower(wallet)
	sess.InitialTransferTx = hash
	sess.ApprovalTxHash = strings.ToLower(approvalTx)
	sess.SubscriptionID = subscriptionID
	sess.CompletedAt = &completed
	m.txOwners[hash] = id
	return nil
}

func (m *MemoryStore) ExpireSessionsBefore(_ context.Context, t time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, s := range m.sessions {
		if s.Status == SessionStatusPending && !s.ExpiresAt.After(t) {
			s.Status = SessionStatusExpired
			// Release any tx reservation; an expired-without-verify session
			// shouldn't permanently block a hash (extraordinarily unlikely
			// to matter, but matches the spec).
			if s.InitialTransferTx != "" {
				if owner := m.txOwners[s.InitialTransferTx]; owner == s.ID {
					delete(m.txOwners, s.InitialTransferTx)
				}
			}
			n++
		}
	}
	return n, nil
}

func (m *MemoryStore) DueBefore(_ context.Context, t time.Time, limit int) ([]*Subscription, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var due []*Subscription
	for _, s := range m.subs {
		if s.Status != StatusActive && s.Status != StatusPastDue {
			continue
		}
		// Pending one-time charges always count as due; otherwise check
		// the regular cycle clock.
		if s.PendingChargeAtomic != "" || !s.NextAttemptAt.After(t) {
			cp := *s
			due = append(due, &cp)
		}
	}
	sort.Slice(due, func(i, j int) bool { return due[i].NextAttemptAt.Before(due[j].NextAttemptAt) })
	if limit > 0 && len(due) > limit {
		due = due[:limit]
	}
	return due, nil
}
