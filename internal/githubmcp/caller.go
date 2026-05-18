package githubmcp

import (
	"context"
	"time"

	"github.com/mattcheramie/nomaddev/internal/middleware"
	"github.com/mattcheramie/nomaddev/internal/sandbox"
)

// Caller is the build-tag-agnostic surface middleware.CompositeDispatcher
// depends on. The real *Client (client.go, -tags github) and the build-tagless
// stub (stub.go) both satisfy it, so dispatcher and factory code stay free of
// build tags.
//
// Call returns a sandbox.ExecChunk channel matching the existing dispatcher
// contract — one stdout chunk carrying the tool's JSON result, then one
// terminal exit chunk. This lets wsserver/middleware.go reuse emitChunk +
// emitResult without special-casing GitHub.
type Caller interface {
	// ListTools enumerates the available MCP tools translated into the
	// middleware's ToolSpec shape. Called once at orchestrator startup.
	ListTools(ctx context.Context) ([]middleware.ToolSpec, error)

	// Call invokes one MCP tool. The call.Tool field is the prefixed name
	// (e.g. "github_list_repositories"); implementations strip the prefix
	// before talking to the upstream library.
	Call(ctx context.Context, call middleware.ToolCall, opts middleware.DispatchOptions) (<-chan sandbox.ExecChunk, error)

	// IsDestructive reports whether the named tool mutates remote state
	// (creates/updates/deletes). Used by the factory to auto-populate the
	// approval-required allowlist so every write goes through the existing
	// approval gate without operator config.
	IsDestructive(prefixedName string) bool

	// Close releases any resources held by the underlying server / transport.
	Close() error
}

// Options configures a real *Client (client.go). Held in this file so the
// orchestrator wiring layer can construct it without build-tag-gating the
// caller.
type Options struct {
	// Token is the credential source. Required.
	Token TokenSource

	// BinaryPath is an explicit path to the github-mcp-server binary. When
	// empty, the client falls back to NOMADDEV_GITHUB_MCP_BIN and then to
	// looking up "github-mcp-server" on PATH.
	BinaryPath string

	// Toolsets is the comma-separated allowlist passed to the upstream
	// server. Empty or {"all"} enables every toolset.
	Toolsets []string

	// ReadOnly mirrors the upstream --read-only flag. Defaults to false
	// since the orchestrator's approval gate is the primary safety
	// mechanism; flip to true for an additional belt-and-suspenders layer.
	ReadOnly bool

	// Host is the GitHub API base URL. Empty → https://api.github.com.
	// Override for GitHub Enterprise Server.
	Host string

	// LockdownMode enables the upstream public-repo content guard.
	LockdownMode bool

	// StartTimeout caps the MCP initialize + tools/list handshake. Zero
	// inherits the parent context's deadline (or never times out if it
	// has none).
	StartTimeout time.Duration

	// MaxArgBytes caps a single tool call's JSON-marshaled arguments before
	// they're handed to the upstream MCP subprocess. Zero (the explicit
	// "no limit" value) disables the check. A misbehaving LLM trying to
	// stuff a 100 MB blob into a github_create_pull_request body is
	// rejected with sandbox.ErrBadRequest before the subprocess sees it,
	// so a runaway prompt doesn't OOM the stdio pipe.
	MaxArgBytes int
}
