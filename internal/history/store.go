// Package history persists per-session conversation turns so the NLP
// middleware can replay LLM context across orchestrator restarts. It is a
// distinct concern from the session ring buffer (internal/session): the ring
// buffer is wire-replay for reconnects (minutes); history is LLM-context
// durable storage (days/weeks).
package history

import (
	"context"
	"errors"
	"time"
)

// Role is the speaker on one Turn. The set is small and stable; new entries
// require coordinated changes in the translator and tests.
type Role string

const (
	RoleUser          Role = "user"
	RoleAssistant     Role = "assistant"
	RoleToolCall      Role = "tool_call"
	RoleToolResult    Role = "tool_result"
	RoleSystemSummary Role = "system.summary"
)

// Turn is one entry in a session's conversation thread. Parts is opaque
// outside the translator — it carries the JSON-encoded structured content
// (typically a Gemini Content{Role, Parts[]} blob) so the store doesn't have
// to model SDK-specific shapes.
type Turn struct {
	SID   string
	Idx   int
	Role  Role
	Parts []byte
	TS    time.Time
}

// Store is the persistence abstraction. Implementations must be safe for
// concurrent use from multiple goroutines; Append within one SID is
// serialized to keep turn_idx monotonic.
type Store interface {
	Append(ctx context.Context, t Turn) (idx int, err error)
	LoadWindow(ctx context.Context, sid string, n int) ([]Turn, error)
	Reset(ctx context.Context, sid string) error
	Close() error
}

// ErrInvalidTurn is returned when Append is called with a malformed Turn.
var ErrInvalidTurn = errors.New("history: invalid turn")
