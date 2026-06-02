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
	mu    sync.RWMutex
	plans map[string]*Plan
	subs  map[string]*Subscription // key = lowercased address
}

func NewMemory() *MemoryStore {
	return &MemoryStore{
		plans: make(map[string]*Plan),
		subs:  make(map[string]*Subscription),
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
