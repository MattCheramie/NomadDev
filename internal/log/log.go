// Package log wraps log/slog to give the orchestrator a single, consistent
// JSON logger plus context plumbing for per-connection child loggers.
package log

import (
	"context"
	"io"
	"log/slog"
	"os"
)

type ctxKey struct{}

// New returns a JSON-formatted slog.Logger writing to stderr at the given level.
func New(level slog.Level) *slog.Logger {
	return NewWithWriter(os.Stderr, level)
}

// NewWithWriter is the test-friendly variant of New.
func NewWithWriter(w io.Writer, level slog.Level) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(h)
}

// ParseLevel maps "debug" | "info" | "warn" | "error" to a slog.Level.
// Unknown values fall back to info.
func ParseLevel(s string) slog.Level {
	switch s {
	case "debug", "DEBUG":
		return slog.LevelDebug
	case "warn", "WARN", "warning":
		return slog.LevelWarn
	case "error", "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// WithRequest returns a child logger tagged with a request id.
func WithRequest(l *slog.Logger, reqID string) *slog.Logger {
	return l.With("req_id", reqID)
}

// ContextWithLogger attaches a logger to ctx.
func ContextWithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// FromContext returns the logger attached via ContextWithLogger, or slog.Default.
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}
