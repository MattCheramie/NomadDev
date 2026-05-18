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
		return NewJSONSink(f, fallback), nil
	default:
		return nil, fmt.Errorf("audit: unknown backend %q (want none|stderr|stdout|file)", backend)
	}
}
