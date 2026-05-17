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

// Supported tool names. Phase 3 implements only one.
const (
	ToolExecuteScript = "execute_script"
)

// ExecRequest is the runner-level translation of an EventCommandRequest.
type ExecRequest struct {
	Tool       string
	Args       map[string]any
	WorkingDir string
	Timeout    time.Duration
	Limits     ResourceLimits
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
