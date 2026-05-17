package history

import (
	"context"
	"sync"
)

// MemoryStore is the in-process Store used by tests and by deployments that
// configure NOMADDEV_HISTORY_BACKEND=memory. State is lost on restart.
type MemoryStore struct {
	mu   sync.Mutex
	rows map[string][]Turn // keyed by SID, ordered by Idx
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{rows: make(map[string][]Turn)}
}

// Append assigns the next monotonic idx for sid and stores a copy of t.
func (m *MemoryStore) Append(_ context.Context, t Turn) (int, error) {
	if t.SID == "" || t.Role == "" {
		return 0, ErrInvalidTurn
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := m.rows[t.SID]
	t.Idx = len(rows)
	rows = append(rows, t)
	m.rows[t.SID] = rows
	return t.Idx, nil
}

// LoadWindow returns the last n turns for sid in chronological order. If n is
// non-positive, returns the full thread.
func (m *MemoryStore) LoadWindow(_ context.Context, sid string, n int) ([]Turn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := m.rows[sid]
	if n <= 0 || n >= len(rows) {
		out := make([]Turn, len(rows))
		copy(out, rows)
		return out, nil
	}
	out := make([]Turn, n)
	copy(out, rows[len(rows)-n:])
	return out, nil
}

// Reset drops all turns for sid.
func (m *MemoryStore) Reset(_ context.Context, sid string) error {
	m.mu.Lock()
	delete(m.rows, sid)
	m.mu.Unlock()
	return nil
}

// Close is a no-op.
func (m *MemoryStore) Close() error { return nil }
