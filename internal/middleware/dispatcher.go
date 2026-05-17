package middleware

import (
	"context"
	"fmt"
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

// CompositeDispatcher routes calls by tool name. execute_script goes to the
// sandbox.Runner; everything else goes to the fsops.Engine.
type CompositeDispatcher struct {
	Sandbox sandbox.Runner
	FSOps   *fsops.Engine
}

// NewCompositeDispatcher constructs a dispatcher. Either Sandbox or FSOps
// may be nil — Dispatch returns ErrBadRequest if the matched backend is
// missing at call time.
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
	default:
		return nil, fmt.Errorf("%w: unknown tool %q", sandbox.ErrBadRequest, call.Tool)
	}
}
