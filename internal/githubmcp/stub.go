//go:build !github

package githubmcp

import (
	"context"
	"errors"
)

// ErrNotBuilt is returned by stub New when the orchestrator was compiled
// without the `github` build tag. cmd/orchestrator/main.go surfaces this
// verbatim so the operator sees what's missing.
var ErrNotBuilt = errors.New("githubmcp: github backend requested but binary built without -tags github")

// New is the stub constructor. Always returns (nil, ErrNotBuilt) so the
// orchestrator gracefully skips GitHub wiring when no tag was set. The
// real *Client (client.go, //go:build github) implements the full Caller
// interface; this stub doesn't need a concrete type because callers check
// the error before touching the returned value.
func New(_ context.Context, _ Options) (Caller, error) {
	return nil, ErrNotBuilt
}
