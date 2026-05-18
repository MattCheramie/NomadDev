package middleware

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// Approver decides whether a tool call needs human approval and waits for
// the answer. Implementations must be safe for concurrent use from multiple
// per-intent goroutines.
type Approver interface {
	// RequiresApproval reports whether the call needs explicit human consent
	// before dispatch. reason is a short human-readable string surfaced to
	// the client; empty reason means "policy-required".
	RequiresApproval(tool string, args map[string]any) (required bool, reason string)
	// Register reserves a slot for requestID before the request envelope is
	// sent. Must be called before Await; calling Await on an unregistered id
	// returns an immediate error.
	Register(requestID string)
	// Signal records the grant/deny outcome for requestID. Safe to call from
	// the wsserver router goroutine.
	Signal(requestID string, granted bool)
	// Cancel releases the slot. Safe to call multiple times.
	Cancel(requestID string)
	// Await blocks until Signal lands for requestID, ctx fires, or the
	// per-Approver timeout elapses.
	Await(ctx context.Context, requestID string) (granted bool, err error)
}

// Sentinel errors from Await.
var (
	ErrApprovalDenied    = errors.New("middleware: approval denied")
	ErrApprovalTimeout   = errors.New("middleware: approval timeout")
	ErrApprovalUnknownID = errors.New("middleware: unknown approval id")
)

// PolicyApprover is the default Approver. It auto-grants when AutoGrant is
// true; otherwise it requires explicit approval for any tool listed in
// RequiredTools.
type PolicyApprover struct {
	requiredTools map[string]struct{}
	autoGrant     bool
	timeout       time.Duration

	mu      sync.Mutex
	pending map[string]chan bool
}

// NewPolicyApprover builds the default Approver from config.
func NewPolicyApprover(requiredTools []string, autoGrant bool, timeout time.Duration) *PolicyApprover {
	set := make(map[string]struct{}, len(requiredTools))
	for _, t := range requiredTools {
		if t != "" {
			set[t] = struct{}{}
		}
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &PolicyApprover{
		requiredTools: set,
		autoGrant:     autoGrant,
		timeout:       timeout,
		pending:       make(map[string]chan bool),
	}
}

// RequiresApproval implements Approver.
func (p *PolicyApprover) RequiresApproval(tool string, _ map[string]any) (bool, string) {
	if p.autoGrant {
		return false, ""
	}
	p.mu.Lock()
	_, ok := p.requiredTools[tool]
	p.mu.Unlock()
	if !ok {
		return false, ""
	}
	switch tool {
	case ToolExecuteScript:
		return true, "runs an arbitrary shell script"
	case ToolWritePatch:
		return true, "writes to the host workspace"
	}
	if strings.HasPrefix(tool, GitHubToolPrefix) {
		return true, "mutates GitHub state"
	}
	return true, "configured policy"
}

// AddRequired extends the required-tools allowlist at runtime. Used by the
// factory to auto-gate every destructive GitHub MCP tool without making the
// operator enumerate ~30 tool names by hand. Safe to call from any goroutine
// because the map is guarded by mu (the same lock that protects pending).
func (p *PolicyApprover) AddRequired(names ...string) {
	if len(names) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, n := range names {
		if n == "" {
			continue
		}
		p.requiredTools[n] = struct{}{}
	}
}

// Register implements Approver.
func (p *PolicyApprover) Register(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.pending[id]; !ok {
		p.pending[id] = make(chan bool, 1)
	}
}

// Signal implements Approver.
func (p *PolicyApprover) Signal(id string, granted bool) {
	p.mu.Lock()
	ch, ok := p.pending[id]
	p.mu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- granted:
	default:
		// Late or duplicate signal — drop without blocking. The first answer
		// always wins.
	}
}

// Cancel implements Approver.
func (p *PolicyApprover) Cancel(id string) {
	p.mu.Lock()
	delete(p.pending, id)
	p.mu.Unlock()
}

// Await implements Approver.
func (p *PolicyApprover) Await(ctx context.Context, id string) (bool, error) {
	p.mu.Lock()
	ch, ok := p.pending[id]
	p.mu.Unlock()
	if !ok {
		return false, ErrApprovalUnknownID
	}
	timer := time.NewTimer(p.timeout)
	defer timer.Stop()
	select {
	case g := <-ch:
		if !g {
			return false, ErrApprovalDenied
		}
		return true, nil
	case <-timer.C:
		return false, ErrApprovalTimeout
	case <-ctx.Done():
		return false, ctx.Err()
	}
}
