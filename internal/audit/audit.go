// Package audit emits structured security-relevant events on a sink
// that's separate from the per-session replay buffer. Operators ship
// the audit stream to a SIEM / syslog / Loki for retention without
// having to grep the orchestrator's general log or replay store.
//
// Audit events are *additive*: every event is also (or will be) visible
// in the relevant component's structured slog stream, but audit gives
// security tooling a single, stable JSON-Lines schema to consume.
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Event kinds. Stable strings — downstream alerting depends on them.
const (
	KindWSConnect     = "ws.connect"
	KindWSAuthFailed  = "ws.auth_failed"
	KindAuthRefresh   = "auth.refresh"
	KindAuthRevoke    = "auth.revoke"
	KindApprovalGrant = "approval.granted"
	KindApprovalDeny  = "approval.denied"
	KindDaemonStart   = "daemon.start"
	KindDaemonStop    = "daemon.stop"
	KindConfigChange  = "config.change"
)

// Outcome values for the Outcome field. Stable strings.
const (
	OutcomeOK    = "ok"
	OutcomeError = "error"
	OutcomeDeny  = "deny"
)

// Event is the structured wire shape every Sink emits.
type Event struct {
	Time    time.Time      `json:"time"`
	Kind    string         `json:"kind"`
	Outcome string         `json:"outcome,omitempty"`
	Sub     string         `json:"sub,omitempty"`     // JWT subject (user id)
	Sid     string         `json:"sid,omitempty"`     // session id
	Tool    string         `json:"tool,omitempty"`    // tool name (approval events)
	Remote  string         `json:"remote,omitempty"`  // remote addr (best-effort)
	JTI     string         `json:"jti,omitempty"`     // token id (auth events)
	Message string         `json:"message,omitempty"` // free-form human-readable
	Extras  map[string]any `json:"extras,omitempty"`  // open-ended; keep keys stable when used
}

// Sink consumes audit events. Implementations must be safe for
// concurrent use. Errors writing an event are logged and dropped —
// audit must never block or fail the action it's recording.
type Sink interface {
	Log(ctx context.Context, e Event)
	Close() error
}

// NoopSink discards every event. Returned by Open when the backend is
// "none" — pre-Phase-8.5 behavior.
type NoopSink struct{}

// Log implements Sink.
func (NoopSink) Log(context.Context, Event) {}

// Close implements Sink.
func (NoopSink) Close() error { return nil }

// JSONSink writes one JSON-Lines record per event to w. Each line is
// self-contained and parseable independently — pipe it straight into
// `jq`, Loki's promtail, or a SIEM agent. The optional fallback logger
// receives write failures so operators can correlate dropped events.
type JSONSink struct {
	mu       sync.Mutex
	w        io.Writer
	closer   io.Closer
	fallback *slog.Logger
	// path is set by Open() when backend=file so Reopen() can
	// reopen the same path on SIGHUP. Empty for stderr/stdout/io.Writer
	// sinks — Reopen on those is a no-op.
	path string
}

// NewJSONSink returns a JSONSink that writes to w. If w implements
// io.Closer, Close will close it; otherwise Close is a no-op.
func NewJSONSink(w io.Writer, fallback *slog.Logger) *JSONSink {
	s := &JSONSink{w: w, fallback: fallback}
	if c, ok := w.(io.Closer); ok {
		s.closer = c
	}
	return s
}

// Log implements Sink. Always sets e.Time if the caller left it zero so
// downstream consumers can rely on the field being present.
func (s *JSONSink) Log(_ context.Context, e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	} else {
		e.Time = e.Time.UTC()
	}
	b, err := json.Marshal(e)
	if err != nil {
		if s.fallback != nil {
			s.fallback.Warn("audit: marshal event", "err", err, "kind", e.Kind)
		}
		return
	}
	b = append(b, '\n')
	s.mu.Lock()
	_, werr := s.w.Write(b)
	s.mu.Unlock()
	if werr != nil && s.fallback != nil {
		s.fallback.Warn("audit: write event", "err", werr, "kind", e.Kind)
	}
}

// Close implements Sink.
func (s *JSONSink) Close() error {
	if s.closer == nil {
		return nil
	}
	return s.closer.Close()
}

// Reopen closes and re-opens the underlying file at the same path.
// Intended for SIGHUP-driven log rotation: external tooling
// (logrotate) renames audit.log → audit.log.1, then HUPs the
// orchestrator; we re-open at the original path and continue
// writing to a fresh fd. Non-file sinks (stderr / stdout / a
// plain io.Writer) are a no-op — the caller can still invoke
// Reopen unconditionally.
func (s *JSONSink) Reopen() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" {
		return nil
	}
	if s.closer != nil {
		_ = s.closer.Close()
	}
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("audit: reopen %q: %w", s.path, err)
	}
	s.w = f
	s.closer = f
	return nil
}

// Reopener is implemented by Sinks that support post-rotation
// reopen. NoopSink doesn't implement this (Reopen would have
// nothing to do); use the type-assertion idiom on the cmd/orchestrator
// side to call it conditionally.
type Reopener interface {
	Reopen() error
}

// Open builds a Sink per (backend, path). Backends:
//
//   - "" / "none"  — NoopSink (silent; pre-Phase-8.5 default).
//   - "stderr"     — JSON-Lines on os.Stderr; operator can `grep` for the
//     "kind" field, or set NOMADDEV_LOG_LEVEL=warn to silence general
//     log noise.
//   - "stdout"     — JSON-Lines on os.Stdout for sidecar log shippers
//     that read the orchestrator's stdout.
//   - "file"       — JSON-Lines appended to path. The directory is
//     created if missing (mode 0o700). Existing content is preserved.
//
// fallback is the slog the JSONSink uses to report its own write
// failures; pass nil to silence those.
func Open(backend, path string, fallback *slog.Logger) (Sink, error) {
	switch backend {
	case "", "none":
		return NoopSink{}, nil
	case "stderr":
		return NewJSONSink(os.Stderr, fallback), nil
	case "stdout":
		return NewJSONSink(os.Stdout, fallback), nil
	case "file":
		if path == "" {
			return nil, errors.New("audit: backend=file requires NOMADDEV_AUDIT_PATH")
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("audit: mkdir %q: %w", filepath.Dir(path), err)
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, fmt.Errorf("audit: open %q: %w", path, err)
		}
		s := NewJSONSink(f, fallback)
		// Stash the path so Reopen() can use it on SIGHUP. Stderr /
		// stdout sinks leave path empty; Reopen on those is a no-op.
		s.path = path
		return s, nil
	default:
		return nil, fmt.Errorf("audit: unknown backend %q (want none|stderr|stdout|file)", backend)
	}
}
