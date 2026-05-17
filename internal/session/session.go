// Package session tracks per-SID state on the orchestrator so a reconnecting
// client can pick up at the last event id it observed.
package session

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/mattcheramie/nomaddev/internal/event"
)

// Store is the abstraction the orchestrator depends on. MemoryStore implements
// it; a persistent backend can be slotted in later without changes upstream.
type Store interface {
	Get(sid string) *Session
	GetOrCreate(sid string) *Session
	Drop(sid string)
}

// Session is the per-SID record. Concurrent access is safe.
type Session struct {
	SID       string
	CreatedAt time.Time

	mu       sync.Mutex
	lastSeen time.Time
	buffer   *RingBuffer

	// persist, if non-nil, is invoked after each Append outside the per-session
	// mutex so a backing store can write the envelope through to durable storage
	// (e.g. SQLite). MemoryStore leaves this nil.
	persist func(env event.Envelope, size int)
}

// Touch updates the last-seen timestamp.
func (s *Session) Touch(now time.Time) {
	s.mu.Lock()
	s.lastSeen = now
	s.mu.Unlock()
}

// LastSeen returns the last activity timestamp.
func (s *Session) LastSeen() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSeen
}

// Append records an outbound envelope in the replay buffer. If a persist hook
// is installed (see SQLiteStore), it is invoked outside the per-session mutex.
func (s *Session) Append(env event.Envelope, size int) {
	s.mu.Lock()
	s.buffer.Push(env, size)
	s.lastSeen = time.Now().UTC()
	persist := s.persist
	s.mu.Unlock()
	if persist != nil {
		persist(env, size)
	}
}

// EventsSince returns the envelopes that arrived strictly after lastID.
// stale=true means the buffer has rolled past lastID — the client must reset.
func (s *Session) EventsSince(lastID string) (events []event.Envelope, stale bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buffer.Since(lastID)
}

// BufferBounds returns the first and last buffered envelope ids (for diagnostics).
func (s *Session) BufferBounds() (first, last string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buffer.FirstID(), s.buffer.LastID()
}

// Clock is replaceable in tests.
type Clock func() time.Time

// MemoryStore keeps sessions in process memory. Lost on restart — see the
// package README for the persistent-store TODO.
type MemoryStore struct {
	bufferSize int
	maxBytes   int
	now        Clock

	mu       sync.Mutex
	sessions map[string]*Session
}

// NewMemoryStore returns a store with the given ring buffer caps.
func NewMemoryStore(bufferSize, maxBytes int) *MemoryStore {
	return &MemoryStore{
		bufferSize: bufferSize,
		maxBytes:   maxBytes,
		now:        func() time.Time { return time.Now().UTC() },
		sessions:   make(map[string]*Session),
	}
}

// SetClock is for tests.
func (m *MemoryStore) SetClock(c Clock) {
	m.mu.Lock()
	m.now = c
	m.mu.Unlock()
}

// Get returns the session for sid, or nil.
func (m *MemoryStore) Get(sid string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[sid]
}

// GetOrCreate is idempotent — concurrent callers see the same *Session.
func (m *MemoryStore) GetOrCreate(sid string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[sid]; ok {
		s.mu.Lock()
		s.lastSeen = m.now()
		s.mu.Unlock()
		return s
	}
	s := &Session{
		SID:       sid,
		CreatedAt: m.now(),
		lastSeen:  m.now(),
		buffer:    NewRingBuffer(m.bufferSize, m.maxBytes),
	}
	m.sessions[sid] = s
	return s
}

// Drop removes a session.
func (m *MemoryStore) Drop(sid string) {
	m.mu.Lock()
	delete(m.sessions, sid)
	m.mu.Unlock()
}

// SweepIdle drops sessions whose lastSeen is older than now-ttl.
// Returns the number of sessions dropped.
func (m *MemoryStore) SweepIdle(ttl time.Duration) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := m.now().Add(-ttl)
	dropped := 0
	for sid, s := range m.sessions {
		if s.LastSeen().Before(cutoff) {
			delete(m.sessions, sid)
			dropped++
		}
	}
	return dropped
}

// RunJanitor calls SweepIdle on a ticker until ctx is cancelled. Intended to
// be launched in its own goroutine by the orchestrator.
func (m *MemoryStore) RunJanitor(ctx context.Context, interval, ttl time.Duration, log *slog.Logger) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if dropped := m.SweepIdle(ttl); dropped > 0 && log != nil {
				log.Info("session: janitor swept", "dropped", dropped)
			}
		}
	}
}
