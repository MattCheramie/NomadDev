// Package githubmcp embeds the official github-mcp-server as an in-process
// tool backend for the orchestrator's NLP middleware. The default build
// (no build tag) ships a stub so binaries stay slim; building with
// `-tags github` pulls in the real implementation.
//
// The wsserver / middleware layer talks to this package through the Caller
// interface; the concrete *Client (in client.go) lives behind the build tag.
package githubmcp

import (
	"context"
	"errors"
	"os"
)

// ErrNoToken is returned by a TokenSource when no credential is available
// for the request's identity (env var unset, per-user PAT not stored, etc.).
var ErrNoToken = errors.New("githubmcp: no token available")

// TokenSource yields a GitHub credential for a single tool call. The context
// carries any per-session identity (SID, sub) so future implementations can
// look up per-user PATs without changing the dispatcher signature.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// EnvTokenSource reads the credential from an environment variable on every
// call. The default Var is NOMADDEV_GITHUB_TOKEN; rotating the variable +
// re-execing the orchestrator picks up the new value without any cache.
type EnvTokenSource struct {
	Var string
}

// Token implements TokenSource.
func (e EnvTokenSource) Token(_ context.Context) (string, error) {
	name := e.Var
	if name == "" {
		name = "NOMADDEV_GITHUB_TOKEN"
	}
	v := os.Getenv(name)
	if v == "" {
		return "", ErrNoToken
	}
	return v, nil
}

// StaticTokenSource yields a hardcoded value. Useful in tests; do not use in
// production code paths since the token cannot be rotated.
type StaticTokenSource struct {
	Value string
}

// Token implements TokenSource.
func (s StaticTokenSource) Token(_ context.Context) (string, error) {
	if s.Value == "" {
		return "", ErrNoToken
	}
	return s.Value, nil
}
