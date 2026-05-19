package middleware

import (
	"github.com/mattcheramie/nomaddev/internal/event"
)

// MaxErrorReportStderrBytes caps the stderr embedded in a
// SystemErrorReportPayload. Keeps the prompt window (for translator
// feedback) and the wire envelope (for hub escalation) bounded so a runaway
// command can't blow either out.
const MaxErrorReportStderrBytes = 8 * 1024

// ToolResultErrorReportKey is the ToolResult.Output key under which the
// orchestrator stashes a SystemErrorReportPayload before resuming the
// translator. The translator's prompt template is expected to surface this
// to the LLM so it can author a fix as a new command.request.
const ToolResultErrorReportKey = "error_report"

// ShouldAutoRetry reports whether a finished tool-call result is a
// candidate for the auto-recovery loop. The classification mirrors
// wsserver.classifyExit:
//
//   - clean non-zero shell exit (errCode == "" and exitCode != 0):
//     retryable. The LLM may be able to fix the script.
//   - transient sandbox failures (timeout / oom): retryable.
//   - structural failures (bad_request, unauthorized, image_pull) and
//     ctx cancellation: NOT retryable. Another LLM round won't help.
//   - success (exitCode == 0 && errCode == ""): NOT retryable.
func ShouldAutoRetry(exitCode int, errCode string) bool {
	switch errCode {
	case "":
		return exitCode != 0
	case event.SandboxErrTimeout, event.SandboxErrOOM:
		return true
	default:
		return false
	}
}

// BuildErrorReport assembles a SystemErrorReportPayload from a failing
// tool-call result. stderrSrc is the raw stderr the dispatcher captured;
// it is truncated to MaxErrorReportStderrBytes from the tail so the most
// recent (and usually most diagnostic) output is preserved.
//
// attempt is 1-indexed and represents the attempt that just failed.
// maxAttempts is the total number the orchestrator will try before
// escalating (typically RetryBudget.Max()+1 — one initial attempt plus
// the retry budget). Escalated is left zero; the caller sets it when
// emitting the wire envelope.
func BuildErrorReport(
	call ToolCall, exitCode int, errCode, errMsg string,
	stderrSrc []byte, attempt, maxAttempts int,
) event.SystemErrorReportPayload {
	stderr := truncStderr(stderrSrc, MaxErrorReportStderrBytes)
	return event.SystemErrorReportPayload{
		Tool:           call.Tool,
		OriginalCallID: call.ID,
		ExitCode:       exitCode,
		ErrorCode:      errCode,
		ErrorMessage:   errMsg,
		Stderr:         stderr,
		Attempt:        attempt,
		MaxAttempts:    maxAttempts,
	}
}

// truncStderr keeps the trailing `limit` bytes of src, prefixed with a
// short marker so the LLM can tell it was truncated. The tail is more
// useful than the head: shell scripts surface their failing line at the
// end of stderr.
func truncStderr(src []byte, limit int) string {
	if limit <= 0 || len(src) == 0 {
		return ""
	}
	if len(src) <= limit {
		return string(src)
	}
	const marker = "...[truncated]...\n"
	if limit <= len(marker) {
		return string(src[len(src)-limit:])
	}
	tail := src[len(src)-(limit-len(marker)):]
	return marker + string(tail)
}

// RetryBudget tracks consecutive failed tool-call dispatches inside one
// chain (one user.intent turn). Consume() is called after each retryable
// failure; Reset() is called after a success or non-retryable failure so a
// sporadic transient error doesn't burn the budget for the rest of the turn.
//
// Not safe for concurrent use; the orchestration loop is single-goroutine
// per intent.
type RetryBudget struct {
	used int
	max  int
}

// NewRetryBudget returns a RetryBudget with cap max. max < 0 is treated as 0.
func NewRetryBudget(max int) *RetryBudget {
	if max < 0 {
		max = 0
	}
	return &RetryBudget{max: max}
}

// Reset returns the budget to its full size. Called after any success or
// non-retryable failure so the chain doesn't accumulate retry pressure
// across unrelated tool calls in a multi-step turn.
func (b *RetryBudget) Reset() {
	if b == nil {
		return
	}
	b.used = 0
}

// Consume accounts for one retryable failure and reports whether the chain
// still has budget for another attempt. Returns false the first time the
// caller has exhausted the budget — the caller should then escalate
// instead of resuming.
func (b *RetryBudget) Consume() (remaining bool) {
	if b == nil {
		return false
	}
	if b.used >= b.max {
		// Budget already exhausted at entry; still count this consumption
		// so Attempt() reflects the failing attempt's index.
		b.used++
		return false
	}
	b.used++
	return true
}

// Attempt returns the 1-indexed number of the most recent failing attempt
// (i.e. how many failures have been Consume()d in the current chain).
// Before the first Consume() it returns 1 — the call that is about to be
// attempted is the first.
func (b *RetryBudget) Attempt() int {
	if b == nil {
		return 1
	}
	if b.used == 0 {
		return 1
	}
	return b.used
}

// Max returns the configured retry cap (the value passed to NewRetryBudget).
func (b *RetryBudget) Max() int {
	if b == nil {
		return 0
	}
	return b.max
}
