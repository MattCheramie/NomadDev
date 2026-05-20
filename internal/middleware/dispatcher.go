package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mattcheramie/nomaddev/internal/fsops"
	"github.com/mattcheramie/nomaddev/internal/history"
	"github.com/mattcheramie/nomaddev/internal/sandbox"
)

// applyVerifyChunkBytes is the slice size used when re-chunking the
// apply_code_patch JSON result onto the composed verify_command stream.
// Matches fsops.emitChunkBytes so a consumer downstream sees the same
// frame cadence whether the patch was verified or not.
const applyVerifyChunkBytes = 16 * 1024

// DispatchOptions threads per-call limits into the dispatcher.
type DispatchOptions struct {
	WorkingDir string
	Timeout    time.Duration
	// SessionID identifies the calling session. The sandbox runner
	// uses this to scope its bind-mounted workspace (Phase 10.2)
	// when PerSessionWorkspace is enabled; empty is back-compat.
	SessionID     string
	SandboxLimits sandbox.ResourceLimits
	FSOpsLimits   fsops.Limits
	// MaxResultBytes caps the structured-tool envelope returned by
	// search_syntax (and shared with the GitHub MCP backend's own cap
	// at internal/githubmcp/client.go). Sourced from
	// NOMADDEV_GITHUB_MAX_RESULT_BYTES; 0 = unlimited. Ignored by
	// execute_script / fsops tools which carry their own per-op caps.
	MaxResultBytes int
	// Mode forwards the per-turn UserIntentPayload.Mode. When set to
	// "audit" Dispatch refuses any base tool IsMutatingBaseTool reports
	// true for; github_* tools are gated upstream in wsserver since the
	// destructiveness predicate is wired through Service.
	Mode string
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
// sandbox.Runner; the four fsops tools go to the fsops.Engine; pin_file /
// unpin_file go to the persistent reference buffer; anything prefixed with
// "github_" goes to the GitHub MCP backend.
type CompositeDispatcher struct {
	Sandbox sandbox.Runner
	FSOps   *fsops.Engine
	GitHub  GitHubCaller
	// Pins backs the pin_file / unpin_file tools. May be nil — Dispatch
	// returns ErrBadRequest for those tools when it is. Set by NewService
	// after construction, the same way GitHub is wired.
	Pins *history.ReferenceBuffer
}

// NewCompositeDispatcher constructs a dispatcher. Any of Sandbox / FSOps /
// GitHub / Pins may be nil — Dispatch returns ErrBadRequest if the matched
// backend is missing at call time.
func NewCompositeDispatcher(r sandbox.Runner, fs *fsops.Engine) *CompositeDispatcher {
	return &CompositeDispatcher{Sandbox: r, FSOps: fs}
}

// Dispatch implements ToolDispatcher.
func (c *CompositeDispatcher) Dispatch(ctx context.Context, call ToolCall, opts DispatchOptions) (<-chan sandbox.ExecChunk, error) {
	if opts.Mode == ModeAudit && IsMutatingBaseTool(call.Tool) {
		return nil, fmt.Errorf("%w: tool %q is disabled in audit mode", sandbox.ErrBadRequest, call.Tool)
	}
	switch call.Tool {
	case ToolExecuteScript, ToolSearchSyntax:
		if c.Sandbox == nil {
			return nil, fmt.Errorf("%w: sandbox runner not configured", sandbox.ErrBadRequest)
		}
		return c.Sandbox.Exec(ctx, sandbox.ExecRequest{
			Tool:           call.Tool,
			Args:           call.Args,
			WorkingDir:     opts.WorkingDir,
			Timeout:        opts.Timeout,
			Limits:         opts.SandboxLimits,
			SessionID:      opts.SessionID,
			MaxResultBytes: opts.MaxResultBytes,
		})
	case ToolReadFile, ToolListDir, ToolWritePatch, ToolApplyCodePatch:
		if c.FSOps == nil {
			return nil, fmt.Errorf("%w: fsops engine not configured", sandbox.ErrBadRequest)
		}
		// Phase 12.2: attach the calling session's id to ctx so the
		// engine can scope path resolution per SID when its
		// PerSession knob is on. No-op when SessionID is empty
		// (legacy callers / cmd/sandbox direct path) or when the
		// engine isn't in per-session mode.
		fsCtx := fsops.WithSessionID(ctx, opts.SessionID)
		// Phase 14: apply_code_patch may carry a verify_command that
		// runs in the sandbox after the patch lands; a non-zero exit
		// rolls the file back. The non-verify path stays on the plain
		// fsops channel so callers without a sandbox runner keep
		// working as before.
		if call.Tool == ToolApplyCodePatch {
			if vc, _ := call.Args["verify_command"].(string); vc != "" {
				if c.Sandbox == nil {
					return nil, fmt.Errorf("%w: verify_command requires sandbox runner", sandbox.ErrBadRequest)
				}
				return c.applyCodePatchWithVerify(ctx, fsCtx, call, opts, vc)
			}
		}
		return c.FSOps.Run(fsCtx, fsops.Call{Tool: call.Tool, Args: call.Args}, opts.FSOpsLimits)
	case ToolPinFile:
		if c.Pins == nil {
			return nil, fmt.Errorf("%w: reference buffer not configured", sandbox.ErrBadRequest)
		}
		if c.FSOps == nil {
			return nil, fmt.Errorf("%w: fsops engine not configured", sandbox.ErrBadRequest)
		}
		return c.pinFile(ctx, call, opts), nil
	case ToolUnpinFile:
		if c.Pins == nil {
			return nil, fmt.Errorf("%w: reference buffer not configured", sandbox.ErrBadRequest)
		}
		return c.unpinFile(call, opts), nil
	}
	if strings.HasPrefix(call.Tool, GitHubToolPrefix) {
		if c.GitHub == nil {
			return nil, fmt.Errorf("%w: github backend not configured", sandbox.ErrBadRequest)
		}
		return c.GitHub.Call(ctx, call, opts)
	}
	return nil, fmt.Errorf("%w: unknown tool %q", sandbox.ErrBadRequest, call.Tool)
}

// applyCodePatchWithVerify is the Phase 14 composition of
// apply_code_patch + sandbox-side verify + on-failure rollback. The
// resulting channel mirrors the sandbox.Runner contract:
//
//   - On verify success: stdout carries the apply_code_patch JSON
//     envelope followed by the verify command's stdout; stderr carries
//     the verify command's stderr; the terminal chunk is exit 0.
//   - On verify failure: the same stdout/stderr stream, then the file
//     is restored from its pre-edit snapshot, a rollback notification
//     is appended to stderr, and the terminal chunk carries the verify
//     command's non-zero exit code with no SandboxErr* code — so the
//     consumeStage recovery loop classifies it as a retryable shell
//     failure and feeds the verify stderr back to the translator.
//   - On verify dispatch error or sandbox-side failure (timeout/oom):
//     the patch is rolled back and the terminal chunk carries the
//     underlying error so wsserver classifies it correctly.
func (c *CompositeDispatcher) applyCodePatchWithVerify(
	ctx context.Context, fsCtx context.Context, call ToolCall,
	opts DispatchOptions, verifyCommand string,
) (<-chan sandbox.ExecChunk, error) {
	snapshot, resolvedPath, applyResult, err := c.FSOps.ApplyCodePatchWithSnapshot(
		fsCtx, call.Args, opts.FSOpsLimits)
	if err != nil {
		// The apply itself failed (bad path, no-match, etc.). Surface as a
		// closed channel with the underlying error so wsserver picks the
		// right SandboxErr* code; nothing was written, so no rollback.
		out := make(chan sandbox.ExecChunk, 1)
		out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1, Err: err}
		close(out)
		return out, nil
	}

	out := make(chan sandbox.ExecChunk, 16)
	go func() {
		defer close(out)

		// 1. Emit the apply_code_patch JSON result on stdout, chunked so
		// downstream chunk-size assumptions still hold.
		if body, jerr := json.Marshal(applyResult); jerr == nil {
			for off := 0; off < len(body); off += applyVerifyChunkBytes {
				end := off + applyVerifyChunkBytes
				if end > len(body) {
					end = len(body)
				}
				select {
				case out <- sandbox.ExecChunk{Stream: sandbox.StreamStdout, Data: append([]byte(nil), body[off:end]...)}:
				case <-ctx.Done():
					_ = c.FSOps.RestoreFile(fsCtx, resolvedPath, snapshot)
					out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1, Err: ctx.Err()}
					return
				}
			}
		}

		// 2. Dispatch verify_command to the sandbox as an execute_script run.
		// The shell is bash; the command is the raw string the model
		// supplied — same semantics as a user invocation of execute_script.
		verifyCh, verr := c.Sandbox.Exec(ctx, sandbox.ExecRequest{
			Tool:           ToolExecuteScript,
			Args:           map[string]any{"shell": "bash", "script": verifyCommand},
			WorkingDir:     opts.WorkingDir,
			Timeout:        opts.Timeout,
			Limits:         opts.SandboxLimits,
			SessionID:      opts.SessionID,
			MaxResultBytes: opts.MaxResultBytes,
		})
		if verr != nil {
			// Couldn't even start the verify — roll back and surface the
			// dispatch error. wsserver will map this to SandboxErrInternal
			// or SandboxErrBadRequest (both non-retryable, which is right:
			// the LLM can't fix a runner outage).
			if rerr := c.FSOps.RestoreFile(fsCtx, resolvedPath, snapshot); rerr != nil {
				out <- sandbox.ExecChunk{Stream: sandbox.StreamStderr,
					Data: []byte(fmt.Sprintf("verify_command dispatch failed: %v; rollback ALSO failed: %v\n", verr, rerr))}
			} else {
				out <- sandbox.ExecChunk{Stream: sandbox.StreamStderr,
					Data: []byte(fmt.Sprintf("verify_command dispatch failed: %v; rolled back %s\n", verr, applyResult.Path))}
			}
			out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1, Err: verr}
			return
		}

		// 3. Stream verify's stdout/stderr through; capture the exit chunk
		// so we can decide whether to roll back before emitting the
		// terminal frame.
		var verifyExit sandbox.ExecChunk
		gotExit := false
		for chunk := range verifyCh {
			if chunk.Stream == sandbox.StreamExit {
				verifyExit = chunk
				gotExit = true
				continue
			}
			select {
			case out <- chunk:
			case <-ctx.Done():
				_ = c.FSOps.RestoreFile(fsCtx, resolvedPath, snapshot)
				out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1, Err: ctx.Err()}
				return
			}
		}
		if !gotExit {
			// Runner closed without an exit chunk — treat as failure and roll back.
			_ = c.FSOps.RestoreFile(fsCtx, resolvedPath, snapshot)
			out <- sandbox.ExecChunk{Stream: sandbox.StreamStderr,
				Data: []byte(fmt.Sprintf("verify_command runner closed without exit; rolled back %s\n", applyResult.Path))}
			out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1,
				Err: fmt.Errorf("verify_command: runner closed without exit chunk")}
			return
		}

		// 4. Decide rollback. Any non-zero exit OR any runner-side error
		// rolls back; a clean exit 0 keeps the patch.
		if verifyExit.Err != nil || verifyExit.ExitCode != 0 {
			if rerr := c.FSOps.RestoreFile(fsCtx, resolvedPath, snapshot); rerr != nil {
				out <- sandbox.ExecChunk{Stream: sandbox.StreamStderr,
					Data: []byte(fmt.Sprintf("\nverify_command failed (exit=%d); rollback ALSO failed: %v\n", verifyExit.ExitCode, rerr))}
			} else {
				out <- sandbox.ExecChunk{Stream: sandbox.StreamStderr,
					Data: []byte(fmt.Sprintf("\nverify_command failed (exit=%d); rolled back %s\n", verifyExit.ExitCode, applyResult.Path))}
			}
		}
		out <- verifyExit
	}()
	return out, nil
}

// pinFile reads path through the fsops read_file tool — reusing its
// path-safety, symlink and byte-cap guarantees — and stores the result in
// the persistent reference buffer. The returned channel follows the
// sandbox.Runner contract: a stdout confirmation then exactly one terminal
// exit chunk. A read failure or a buffer-cap rejection surfaces as exit -1
// so wsserver classifies it as a tool-result error.
func (c *CompositeDispatcher) pinFile(
	ctx context.Context, call ToolCall, opts DispatchOptions,
) <-chan sandbox.ExecChunk {
	out := make(chan sandbox.ExecChunk, 2)
	go func() {
		defer close(out)

		path, _ := call.Args["path"].(string)
		if path == "" {
			out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1,
				Err: fmt.Errorf("%w: pin_file requires a non-empty 'path'", sandbox.ErrBadRequest)}
			return
		}

		fsCtx := fsops.WithSessionID(ctx, opts.SessionID)
		readCh, err := c.FSOps.Run(fsCtx,
			fsops.Call{Tool: fsops.ToolReadFile, Args: map[string]any{"path": path}},
			opts.FSOpsLimits)
		if err != nil {
			out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1, Err: err}
			return
		}

		var buf bytes.Buffer
		var readExit sandbox.ExecChunk
		gotExit := false
		for chunk := range readCh {
			switch chunk.Stream {
			case sandbox.StreamStdout:
				buf.Write(chunk.Data)
			case sandbox.StreamExit:
				readExit = chunk
				gotExit = true
			}
		}
		if !gotExit {
			out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1,
				Err: fmt.Errorf("pin_file: read_file closed without an exit chunk")}
			return
		}
		if readExit.Err != nil || readExit.ExitCode != 0 {
			rerr := readExit.Err
			if rerr == nil {
				rerr = fmt.Errorf("%w: read_file exited %d", sandbox.ErrBadRequest, readExit.ExitCode)
			}
			out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1, Err: rerr}
			return
		}

		if perr := c.Pins.Pin(opts.SessionID, path, buf.Bytes()); perr != nil {
			out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1,
				Err: fmt.Errorf("%w: %v", sandbox.ErrBadRequest, perr)}
			return
		}

		msg := fmt.Sprintf("pinned %q (%d bytes) to the persistent reference buffer; "+
			"it will stay at the top of the prompt until unpinned", path, buf.Len())
		out <- sandbox.ExecChunk{Stream: sandbox.StreamStdout, Data: []byte(msg)}
		out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: 0}
	}()
	return out
}

// unpinFile drops path from the reference buffer. Unpinning a path that was
// never pinned is reported as a clean no-op (exit 0), not a failure, so the
// automated-recovery loop is not triggered for a harmless call.
func (c *CompositeDispatcher) unpinFile(call ToolCall, opts DispatchOptions) <-chan sandbox.ExecChunk {
	out := make(chan sandbox.ExecChunk, 2)
	defer close(out)

	path, _ := call.Args["path"].(string)
	if path == "" {
		out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1,
			Err: fmt.Errorf("%w: unpin_file requires a non-empty 'path'", sandbox.ErrBadRequest)}
		return out
	}

	var msg string
	if c.Pins.Unpin(opts.SessionID, path) {
		msg = fmt.Sprintf("unpinned %q from the persistent reference buffer", path)
	} else {
		msg = fmt.Sprintf("%q was not pinned; nothing to unpin", path)
	}
	out <- sandbox.ExecChunk{Stream: sandbox.StreamStdout, Data: []byte(msg)}
	out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: 0}
	return out
}
