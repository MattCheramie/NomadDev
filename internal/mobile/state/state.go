// Package state owns the mobile app's in-memory store and the inbound
// envelope reducer. It mirrors the slices and ingest switch in
// mobile/src/state/store.ts so the native app behaves the same way as the
// React Native SPA from the user's perspective.
//
// Concurrency: the WebSocket session goroutine writes to the Store via
// Update; the UI goroutine reads via Snapshot and reacts to Subscribe
// notifications. All mutations go through a single mutex so neither side
// observes a torn read.
package state

import (
	"sync"

	"github.com/mattcheramie/nomaddev/internal/event"
	"github.com/mattcheramie/nomaddev/internal/wireclient"
)

// Turn is one full request/response cycle — user input plus everything the
// assistant produced in reply (streamed prose plus, eventually, tool calls
// and results). Finished is set when the terminal assistant.message arrives
// or when an error frame closes the turn.
type Turn struct {
	IntentID      string
	UserText      string
	UserImages    []event.ImageInput
	AssistantText string
	Finished      bool
	Error         string
}

// SessionTokens mirrors the per-turn LLM usage panel the mobile Settings
// screen renders. Updated cumulatively across the session, never reset.
type SessionTokens struct {
	Prompt     int64
	Candidates int64
	Total      int64
	CostUSD    float64
}

// State is the immutable snapshot exposed by Store.Snapshot.
type State struct {
	ServerURL       string
	Token           string
	SessionID       string
	Status          wireclient.Status
	Turns           []Turn
	Provider        string
	Model           string
	AvailableModels []string
	SessionTokens   SessionTokens
	LastEventID     string
	LastError       string
}

// Store holds the app's mutable state behind a mutex and notifies
// subscribers on every change. UI code drives layout from Snapshot.
type Store struct {
	mu    sync.RWMutex
	state State
	subs  map[chan struct{}]struct{}
}

// New returns a fresh Store in the idle state.
func New() *Store {
	return &Store{
		state: State{Status: wireclient.StatusIdle},
		subs:  make(map[chan struct{}]struct{}),
	}
}

// Snapshot returns a value copy of the current state. The Turns slice is
// shared by reference; callers must treat it as read-only.
func (s *Store) Snapshot() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// Subscribe returns a channel that receives a non-blocking signal whenever
// the state changes, plus an unsubscribe func. The channel is buffered to 1
// so a slow subscriber only ever misses redundant signals — the next read
// will see the latest state.
func (s *Store) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}
}

// Update runs fn against an addressable State copy and commits it as the
// new state atomically. Subscribers are notified once.
func (s *Store) Update(fn func(*State)) {
	s.mu.Lock()
	fn(&s.state)
	subs := make([]chan struct{}, 0, len(s.subs))
	for ch := range s.subs {
		subs = append(subs, ch)
	}
	s.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// SetCredentials persists the server URL + token the user typed on the
// Onboard screen. Calling this with empty values clears them ("sign out").
func (s *Store) SetCredentials(serverURL, token string) {
	s.Update(func(st *State) {
		st.ServerURL = serverURL
		st.Token = token
		if token == "" {
			st.SessionID = ""
			st.LastEventID = ""
			st.Turns = nil
			st.Status = wireclient.StatusIdle
		}
	})
}

// SetStatus updates the connection status. Called from the wireclient.Session
// status callback.
func (s *Store) SetStatus(st wireclient.Status) {
	s.Update(func(s *State) { s.Status = st })
}

// SetLastError records a human-readable error string surfaced on the Onboard
// screen and the chat composer area. Pass "" to clear.
func (s *Store) SetLastError(msg string) {
	s.Update(func(st *State) { st.LastError = msg })
}

// RecordSentIntent appends a new outbound turn to the history. The mobile
// composer calls this on submit so the user's bubble renders immediately,
// before the orchestrator's first assistant.chunk arrives.
func (s *Store) RecordSentIntent(intentID, text string, images []event.ImageInput) {
	s.Update(func(st *State) {
		st.Turns = append(st.Turns, Turn{
			IntentID:   intentID,
			UserText:   text,
			UserImages: images,
		})
	})
}
