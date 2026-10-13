package store

import (
	"context"
	"sync"
	"time"
)

// MemoryStore is an in-process Store used for tests and running without Redis.
type MemoryStore struct {
	mu    sync.RWMutex
	links map[string]Link
	now   func() time.Time
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{links: make(map[string]Link), now: time.Now}
}

func (m *MemoryStore) Create(_ context.Context, l Link) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.links[l.Code]; ok && !m.expired(existing) {
		return ErrExists
	}
	m.links[l.Code] = l
	return nil
}

func (m *MemoryStore) Get(_ context.Context, code string) (Link, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	l, ok := m.links[code]
	if !ok || m.expired(l) {
		return Link{}, ErrNotFound
	}
	return l, nil
}

func (m *MemoryStore) IncrClicks(_ context.Context, code, linkID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.links[code]
	if !ok || m.expired(l) || l.ID != linkID {
		return ErrNotFound
	}
	l.Clicks++
	m.links[code] = l
	return nil
}

func (m *MemoryStore) Ping(context.Context) error { return nil }

func (m *MemoryStore) expired(l Link) bool {
	return l.ExpiresAt != nil && !m.now().Before(*l.ExpiresAt)
}
