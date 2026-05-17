//go:build !github

package githubmcp

import (
	"context"
	"errors"

	"github.com/mattcheramie/nomaddev/internal/middleware"
	"github.com/mattcheramie/nomaddev/internal/sandbox"
)

// ErrNotBuilt is returned by stub New when the orchestrator was compiled
// without the `github` build tag. cmd/orchestrator/main.go surfaces this
// verbatim so the operator sees what's missing.
var ErrNotBuilt = errors.New("githubmcp: github backend requested but binary built without -tags github")

// stubClient implements Caller for default builds. Every method is a no-op
// returning a "not built" error. The dispatcher routes around it because
// NewService sees New(...) return nil and leaves CompositeDispatcher.GitHub
// nil; this type exists only as a fallback in case someone wires it in.
type stubClient struct{}

// New is the stub constructor. Always returns (nil, ErrNotBuilt) so the
// orchestrator gracefully skips GitHub wiring when no tag was set.
func New(_ context.Context, _ Options) (Caller, error) {
	return nil, ErrNotBuilt
}

func (stubClient) ListTools(_ context.Context) ([]middleware.ToolSpec, error) {
	return nil, ErrNotBuilt
}

func (stubClient) Call(_ context.Context, _ middleware.ToolCall, _ middleware.DispatchOptions) (<-chan sandbox.ExecChunk, error) {
	return nil, ErrNotBuilt
}

func (stubClient) IsDestructive(_ string) bool { return false }

func (stubClient) Close() error { return nil }
