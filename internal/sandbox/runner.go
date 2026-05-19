package sandbox

import (
	"context"
	"errors"
	"time"
)

// Runner executes one tool invocation inside an isolated environment and
// streams output chunks back over the returned channel.
//
// Contract:
//   - On a non-nil error from Exec, no channel is returned and the caller
//     handles the failure synchronously.
//   - On a nil error, the Runner emits zero or more chunks with
//     Stream==StreamStdout or Stream==StreamStderr, then exactly one chunk
//     with Stream==StreamExit, then closes the channel.
//   - The exit chunk carries ExitCode (real process exit on clean termination,
//     -1 otherwise) and an Err that the handler maps to a SandboxErr* code.
//   - All goroutines started by Exec must exit when ctx is cancelled. The
//     Runner is responsible for synthesizing an exit chunk on cancellation.
type Runner interface {
	Exec(ctx context.Context, req ExecRequest) (<-chan ExecChunk, error)
}

// Stream identifiers on an ExecChunk. The wire-side strings (used in the
// CommandChunkPayload envelope) live in package event; these are local
// constants so the sandbox package has no dependency on the event package.
const (
	StreamStdout = "stdout"
	StreamStderr = "stderr"
	StreamExit   = "exit"
)

// Supported tool names. Phase 3 implemented `execute_script`; Phase 12
// adds `search_syntax`, a structural ast-grep query the worker runs by
// shelling out to `sg` inside the same ephemeral container.
const (
	ToolExecuteScript = "execute_script"
	ToolSearchSyntax  = "search_syntax"
)

// ExecRequest is the runner-level translation of an EventCommandRequest.
type ExecRequest struct {
	Tool       string
	Args       map[string]any
	WorkingDir string
	Timeout    time.Duration
	Limits     ResourceLimits
	// SessionID, when set AND the runner was constructed with
	// PerSessionWorkspace=true, scopes the bind-mounted workspace
	// to <WorkspaceDir>/<SessionID> so two sessions can't see each
	// other's files. Empty (default) preserves the shared-root
	// behavior — back-compat for direct callers like cmd/sandbox.
	SessionID string
	// MaxResultBytes caps the structured-tool envelope (today only
	// search_syntax) so a single result can't blow the model's context
	// window. 0 = unlimited. Sourced from NOMADDEV_GITHUB_MAX_RESULT_BYTES;
	// the cap is shared with the GitHub MCP backend rather than introducing
	// a second knob. Ignored by execute_script, which streams raw bytes.
	MaxResultBytes int
}

// ResourceLimits maps to Docker HostConfig fields. Zero values disable the
// corresponding limit.
type ResourceLimits struct {
	CPUNanos    int64
	MemoryBytes int64
	PidsLimit   int64
}

// ExecChunk is one frame on the stream from Runner to caller.
//
//	{Stream: stdout, Data: ...}*
//	{Stream: stderr, Data: ...}*           (interleaved)
//	{Stream: exit,   ExitCode: N, Err: ...} (exactly once, last)
type ExecChunk struct {
	Stream   string
	Data     []byte
	ExitCode int
	Err      error
}

// ErrBadRequest is returned (or wrapped) from Exec when the request is
// malformed (unknown tool, missing or wrong-typed args).
var ErrBadRequest = errors.New("sandbox: bad request")

// ErrCanceled is the sentinel Runners use on the exit chunk when ctx is
// cancelled by the caller (typically a client disconnect).
var ErrCanceled = errors.New("sandbox: canceled")

// ErrOOM is set as the exit chunk Err when the container was killed by the
// kernel's OOM killer.
var ErrOOM = errors.New("sandbox: out of memory")

// ErrImagePull is set as the exit chunk Err when ImagePull failed before the
// container could start. Wrap it with %w so the caller can inspect the
// underlying engine error via errors.Unwrap.
var ErrImagePull = errors.New("sandbox: image pull failed")

// sanitizeSID returns sid with unsafe path characters replaced so it
// can be joined into a workspace path. The orchestrator already
// constrains SIDs to JWT-claim shapes (URL-safe-ish), but defense
// in depth: drop anything that could traverse the workspace root,
// and cap length so a maliciously-crafted SID can't blow PATH_MAX.
func sanitizeSID(sid string) string {
	if sid == "" {
		return ""
	}
	const maxLen = 64
	b := make([]byte, 0, len(sid))
	for i := 0; i < len(sid) && len(b) < maxLen; i++ {
		c := sid[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '-' || c == '_' || c == '.':
			// `.` is allowed once but `..` collapses to `_` so a
			// constructed SID like "../etc" can't escape the root.
			if c == '.' && len(b) > 0 && b[len(b)-1] == '.' {
				b[len(b)-1] = '_'
				b = append(b, '_')
				continue
			}
			b = append(b, c)
		default:
			b = append(b, '_')
		}
	}
	return string(b)
}
