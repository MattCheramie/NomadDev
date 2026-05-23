package wireclient

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mattcheramie/nomaddev/internal/event"
)

// Status is the high-level connection state surfaced to the UI. It matches
// the values used by the React Native SPA so the mobile state layer can use
// the same enumeration.
type Status string

const (
	StatusIdle         Status = "idle"
	StatusConnecting   Status = "connecting"
	StatusOpen         Status = "open"
	StatusClosed       Status = "closed"
	StatusUnauthorized Status = "unauthorized"
)

// SessionConfig parametrises a long-lived auto-reconnecting client.
//
// Dial / dispatch hooks live here rather than as method receivers so the UI
// layer can swap implementations in tests without subclassing.
type SessionConfig struct {
	Dial DialOptions

	// OnStatus is invoked from a background goroutine every time the
	// connection's high-level Status changes. It must not block; UI code
	// should marshal back to its own thread.
	OnStatus func(Status)

	// OnEnvelope is invoked from a background goroutine for every inbound
	// envelope. Same blocking caveat as OnStatus.
	OnEnvelope func(event.Envelope)

	// OnLastEventID is invoked whenever the session observes a new inbound
	// envelope ID. The mobile app persists the most recent value so a
	// reconnect after a process death can still replay missed events via
	// client.hello{last_event_id}. May be nil for tests.
	OnLastEventID func(string)

	// OutboxCap caps the queue of user.intent envelopes buffered while
	// offline. Zero means use the default (64) — matches the RN SPA.
	OutboxCap int

	// ReconnectBase / ReconnectCap define the exponential backoff envelope
	// for the reconnect loop. Zero means use the defaults (1s / 30s).
	ReconnectBase time.Duration
	ReconnectCap  time.Duration

	// ReadTimeout sets a per-frame read deadline. Zero disables it; the
	// orchestrator pings every ~30s so a 60s timeout is the recommended
	// floor. Tests use shorter values.
	ReadTimeout time.Duration

	// LastEventID, if non-empty, is sent in a client.hello immediately after
	// the orchestrator's hello arrives — used to replay events missed during
	// a previous disconnect or process death.
	LastEventID string

	// now is overridable for tests; never set in production.
	now func() time.Time
}

// Session is a long-lived envelope-level WebSocket client. It owns the
// reconnect loop, the offline outbox, and the dispatch of inbound envelopes
// to the OnEnvelope hook. The mobile app exposes one Session per signed-in
// account.
type Session struct {
	cfg SessionConfig

	mu        sync.Mutex
	conn      *Conn
	closed    bool
	outbox    []event.Envelope
	lastEvent string
	statusVal Status
}

// NewSession constructs a Session. Run must be called to drive it; nothing
// happens until then.
func NewSession(cfg SessionConfig) *Session {
	if cfg.OutboxCap <= 0 {
		cfg.OutboxCap = 64
	}
	if cfg.ReconnectBase <= 0 {
		cfg.ReconnectBase = time.Second
	}
	if cfg.ReconnectCap <= 0 {
		cfg.ReconnectCap = 30 * time.Second
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	return &Session{
		cfg:       cfg,
		statusVal: StatusIdle,
		lastEvent: cfg.LastEventID,
	}
}

// Status returns the current connection status.
func (s *Session) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusVal
}

// Send queues an envelope for transmission. If the connection is open it
// goes on the wire immediately; otherwise — for user.intent only — it is
// queued in the outbox (capped at OutboxCap; oldest dropped) and replayed
// after reconnect. Non-user.intent envelopes are rejected with ErrOffline
// when the session is closed so the UI can show a "not connected" state.
func (s *Session) Send(env event.Envelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if s.conn != nil && s.statusVal == StatusOpen {
		if err := s.conn.WriteEnvelope(env); err != nil {
			return err
		}
		return nil
	}
	if env.Type != event.EventUserIntent {
		return ErrOffline
	}
	if len(s.outbox) >= s.cfg.OutboxCap {
		s.outbox = s.outbox[1:]
	}
	s.outbox = append(s.outbox, env)
	return nil
}

// OutboxLen returns the number of envelopes queued for delivery on the next
// reconnect. The mobile Settings screen polls this for its outbox indicator.
func (s *Session) OutboxLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.outbox)
}

// LastEventID returns the most recently observed inbound envelope ID. It
// survives reconnects so client.hello can ask the orchestrator to replay.
func (s *Session) LastEventID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastEvent
}

// Close marks the session as terminating. Run will return on the next loop
// iteration; in-flight envelopes are abandoned.
func (s *Session) Close() {
	s.mu.Lock()
	s.closed = true
	conn := s.conn
	s.conn = nil
	s.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// Run drives the connect / read / reconnect loop. It blocks until ctx is
// cancelled or Close is called. Errors from the dial layer (auth failures
// included) are surfaced via OnStatus(StatusUnauthorized) and OnStatus
// (StatusClosed); Run itself returns nil unless the context errored.
func (s *Session) Run(ctx context.Context) error {
	backoff := s.cfg.ReconnectBase
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return nil
		}
		s.mu.Unlock()

		s.setStatus(StatusConnecting)
		conn, err := Dial(s.cfg.Dial)
		if err != nil {
			if isUnauthorized(err) {
				s.setStatus(StatusUnauthorized)
				return nil
			}
			s.setStatus(StatusClosed)
			if !sleepCtx(ctx, backoff) {
				return ctx.Err()
			}
			backoff = nextBackoff(backoff, s.cfg.ReconnectCap)
			continue
		}

		s.mu.Lock()
		s.conn = conn
		s.mu.Unlock()

		if err := s.readHelloAndResume(conn); err != nil {
			_ = conn.Close()
			s.mu.Lock()
			s.conn = nil
			s.mu.Unlock()
			s.setStatus(StatusClosed)
			if !sleepCtx(ctx, backoff) {
				return ctx.Err()
			}
			backoff = nextBackoff(backoff, s.cfg.ReconnectCap)
			continue
		}

		s.setStatus(StatusOpen)
		backoff = s.cfg.ReconnectBase
		if err := s.drainOutbox(); err != nil {
			_ = conn.Close()
			s.mu.Lock()
			s.conn = nil
			s.mu.Unlock()
			s.setStatus(StatusClosed)
			continue
		}

		s.readLoop(ctx, conn)

		s.mu.Lock()
		s.conn = nil
		s.mu.Unlock()
		_ = conn.Close()
		s.setStatus(StatusClosed)
	}
}

func (s *Session) readHelloAndResume(conn *Conn) error {
	env, err := conn.ReadEnvelope(s.cfg.ReadTimeout)
	if err != nil {
		return fmt.Errorf("read hello: %w", err)
	}
	s.observe(env)

	s.mu.Lock()
	lastID := s.lastEvent
	s.mu.Unlock()
	// The hello we just read counts as the last observed ID; only send a
	// client.hello when we have a strictly older ID to resume from.
	if lastID != "" && lastID != env.ID {
		ch, err := event.NewEnvelope(event.EventClientHello, event.ClientHelloPayload{LastEventID: lastID})
		if err != nil {
			return fmt.Errorf("build client.hello: %w", err)
		}
		if err := conn.WriteEnvelope(ch); err != nil {
			return fmt.Errorf("write client.hello: %w", err)
		}
	}
	return nil
}

func (s *Session) drainOutbox() error {
	s.mu.Lock()
	pending := s.outbox
	s.outbox = nil
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return ErrClosed
	}
	for i, env := range pending {
		if err := conn.WriteEnvelope(env); err != nil {
			// Re-queue the items we did not get to (including the one
			// that failed) so they're retried on the next reconnect.
			// Any items Send queued in the meantime go after.
			s.mu.Lock()
			leftover := pending[i:]
			s.outbox = append(leftover, s.outbox...)
			s.mu.Unlock()
			return err
		}
	}
	return nil
}

func (s *Session) readLoop(ctx context.Context, conn *Conn) {
	for {
		if ctx.Err() != nil {
			return
		}
		env, err := conn.ReadEnvelope(s.cfg.ReadTimeout)
		if err != nil {
			return
		}
		s.observe(env)
	}
}

func (s *Session) observe(env event.Envelope) {
	s.mu.Lock()
	s.lastEvent = env.ID
	s.mu.Unlock()
	if s.cfg.OnLastEventID != nil {
		s.cfg.OnLastEventID(env.ID)
	}
	if s.cfg.OnEnvelope != nil {
		s.cfg.OnEnvelope(env)
	}
}

func (s *Session) setStatus(st Status) {
	s.mu.Lock()
	if s.statusVal == st {
		s.mu.Unlock()
		return
	}
	s.statusVal = st
	s.mu.Unlock()
	if s.cfg.OnStatus != nil {
		s.cfg.OnStatus(st)
	}
}

func nextBackoff(cur, ceiling time.Duration) time.Duration {
	n := cur * 2
	if n > ceiling {
		return ceiling
	}
	return n
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func isUnauthorized(err error) bool {
	// Dial wraps the HTTP status into the error message as `status=NNN`.
	// We match on the canonical 401/403 codes the orchestrator returns
	// from authn middleware; anything else is a transient dial failure.
	msg := err.Error()
	return strings.Contains(msg, "status=401") || strings.Contains(msg, "status=403")
}

// ErrOffline is returned by Send for envelopes that are not eligible for the
// outbox while the session is disconnected.
var ErrOffline = errors.New("wireclient: session offline")

// ErrClosed is returned by Send after Close has been called.
var ErrClosed = errors.New("wireclient: session closed")
