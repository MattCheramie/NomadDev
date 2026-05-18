package middleware

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mattcheramie/nomaddev/internal/fsops"
	"github.com/mattcheramie/nomaddev/internal/sandbox"
)

// DispatchOptions threads per-call limits into the dispatcher.
type DispatchOptions struct {
	WorkingDir    string
	Timeout       time.Duration
	SandboxLimits sandbox.ResourceLimits
	FSOpsLimits   fsops.Limits
}

// ToolDispatcher executes one tool call and streams sandbox.ExecChunk frames
// back. Reusing sandbox.ExecChunk across both backends lets the wsserver
// layer pipe results through one envelope emission path.
type ToolDispatcher interface {
	Dispatch(ctx context.Context, call ToolCall, opts DispatchOptions) (<-chan sandbox.ExecChunk, error)
}

// GitHubCaller is the subset of internal/githubmcp.Caller that the dispatcher
// depends on. Declared here so the middleware package stays free of build tags
// and can compile against both the real client and the no-op stub.
type GitHubCaller interface {
	Call(ctx context.Context, call ToolCall, opts DispatchOptions) (<-chan sandbox.ExecChunk, error)
}

// CompositeDispatcher routes calls by tool name. execute_script goes to the
// sandbox.Runner; the four fsops tools go to the fsops.Engine; anything
// prefixed with "github_" goes to the GitHub MCP backend.
type CompositeDispatcher struct {
	Sandbox sandbox.Runner
	FSOps   *fsops.Engine
	GitHub  GitHubCaller
}

// NewCompositeDispatcher constructs a dispatcher. Any of Sandbox / FSOps /
// GitHub may be nil — Dispatch returns ErrBadRequest if the matched backend
// is missing at call time.
func NewCompositeDispatcher(r sandbox.Runner, fs *fsops.Engine) *CompositeDispatcher {
	return &CompositeDispatcher{Sandbox: r, FSOps: fs}
}

// Dispatch implements ToolDispatcher.
func (c *CompositeDispatcher) Dispatch(ctx context.Context, call ToolCall, opts DispatchOptions) (<-chan sandbox.ExecChunk, error) {
	switch call.Tool {
	case ToolExecuteScript:
		if c.Sandbox == nil {
			return nil, fmt.Errorf("%w: sandbox runner not configured", sandbox.ErrBadRequest)
		}
		return c.Sandbox.Exec(ctx, sandbox.ExecRequest{
			Tool:       sandbox.ToolExecuteScript,
			Args:       call.Args,
			WorkingDir: opts.WorkingDir,
			Timeout:    opts.Timeout,
			Limits:     opts.SandboxLimits,
		})
	case ToolReadFile, ToolListDir, ToolWritePatch:
		if c.FSOps == nil {
			return nil, fmt.Errorf("%w: fsops engine not configured", sandbox.ErrBadRequest)
		}
		return c.FSOps.Run(ctx, fsops.Call{Tool: call.Tool, Args: call.Args}, opts.FSOpsLimits)
	}
	if strings.HasPrefix(call.Tool, GitHubToolPrefix) {
		if c.GitHub == nil {
			return nil, fmt.Errorf("%w: github backend not configured", sandbox.ErrBadRequest)
		}
		return c.GitHub.Call(ctx, call, opts)
	}
	return nil, fmt.Errorf("%w: unknown tool %q", sandbox.ErrBadRequest, call.Tool)
}
