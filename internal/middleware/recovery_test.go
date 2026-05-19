package middleware

import (
	"strings"
	"testing"

	"github.com/mattcheramie/nomaddev/internal/event"
)

func TestShouldAutoRetry(t *testing.T) {
	cases := []struct {
		name     string
		exitCode int
		errCode  string
		want     bool
	}{
		{"clean exit zero", 0, "", false},
		{"clean exit nonzero", 1, "", true},
		{"clean exit -1 no errcode", -1, "", true},
		{"sandbox timeout", -1, event.SandboxErrTimeout, true},
		{"sandbox oom", -1, event.SandboxErrOOM, true},
		{"sandbox bad request", -1, event.SandboxErrBadRequest, false},
		{"sandbox unauthorized", -1, event.SandboxErrUnauthorized, false},
		{"sandbox image pull", -1, event.SandboxErrImagePull, false},
		{"sandbox canceled", -1, event.SandboxErrCanceled, false},
		{"sandbox internal", -1, event.SandboxErrInternal, false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := ShouldAutoRetry(c.exitCode, c.errCode)
			if got != c.want {
				t.Errorf("ShouldAutoRetry(%d, %q) = %v, want %v", c.exitCode, c.errCode, got, c.want)
			}
		})
	}
}

func TestBuildErrorReport_FieldsAndShortStderr(t *testing.T) {
	call := ToolCall{ID: "c1", Tool: "execute_script"}
	got := BuildErrorReport(call, 1, "", "oops", []byte("boom\n"), 2, 3)

	if got.Tool != "execute_script" || got.OriginalCallID != "c1" {
		t.Errorf("identity wrong: %+v", got)
	}
	if got.ExitCode != 1 || got.ErrorCode != "" || got.ErrorMessage != "oops" {
		t.Errorf("error fields wrong: %+v", got)
	}
	if got.Attempt != 2 || got.MaxAttempts != 3 {
		t.Errorf("attempt fields wrong: attempt=%d max=%d", got.Attempt, got.MaxAttempts)
	}
	if got.Stderr != "boom\n" {
		t.Errorf("Stderr = %q, want \"boom\\n\"", got.Stderr)
	}
	if got.Escalated {
		t.Errorf("Escalated must default to false; caller sets it on the wire form")
	}
}

func TestBuildErrorReport_TruncatesStderr(t *testing.T) {
	big := make([]byte, MaxErrorReportStderrBytes*2)
	for i := range big {
		big[i] = 'a'
	}
	copy(big[len(big)-5:], []byte("TAIL!"))

	call := ToolCall{ID: "c2", Tool: "execute_script"}
	got := BuildErrorReport(call, 2, "", "", big, 1, 3)

	if len(got.Stderr) > MaxErrorReportStderrBytes {
		t.Fatalf("Stderr length = %d, want <= %d", len(got.Stderr), MaxErrorReportStderrBytes)
	}
	if !strings.HasSuffix(got.Stderr, "TAIL!") {
		t.Errorf("truncation must keep the tail; got suffix = %q", got.Stderr[len(got.Stderr)-16:])
	}
	if !strings.Contains(got.Stderr, "[truncated]") {
		t.Errorf("truncation marker missing; got = %q", got.Stderr[:32])
	}
}

func TestBuildErrorReport_EmptyStderr(t *testing.T) {
	got := BuildErrorReport(ToolCall{ID: "c3", Tool: "x"}, 1, "", "", nil, 1, 3)
	if got.Stderr != "" {
		t.Errorf("Stderr from nil src = %q, want \"\"", got.Stderr)
	}
}

func TestRetryBudget_ConsumeAndReset(t *testing.T) {
	b := NewRetryBudget(2)
	if b.Max() != 2 {
		t.Errorf("Max() = %d, want 2", b.Max())
	}
	if b.Attempt() != 1 {
		t.Errorf("initial Attempt() = %d, want 1", b.Attempt())
	}
	if ok := b.Consume(); !ok {
		t.Errorf("first Consume on max=2 must return true (remaining)")
	}
	if b.Attempt() != 1 {
		t.Errorf("Attempt after 1st failure = %d, want 1", b.Attempt())
	}
	if ok := b.Consume(); !ok {
		t.Errorf("second Consume on max=2 must return true (remaining)")
	}
	if b.Attempt() != 2 {
		t.Errorf("Attempt after 2nd failure = %d, want 2", b.Attempt())
	}
	if ok := b.Consume(); ok {
		t.Errorf("third Consume on max=2 must return false (exhausted)")
	}
	if b.Attempt() != 3 {
		t.Errorf("Attempt after 3rd failure = %d, want 3", b.Attempt())
	}

	b.Reset()
	if b.Attempt() != 1 {
		t.Errorf("Attempt after Reset = %d, want 1", b.Attempt())
	}
	if ok := b.Consume(); !ok {
		t.Errorf("post-Reset Consume must return true")
	}
}

func TestRetryBudget_ZeroMaxEscalatesImmediately(t *testing.T) {
	b := NewRetryBudget(0)
	if ok := b.Consume(); ok {
		t.Errorf("Consume on max=0 must return false (no budget)")
	}
}

func TestRetryBudget_NegativeMaxClamped(t *testing.T) {
	b := NewRetryBudget(-5)
	if b.Max() != 0 {
		t.Errorf("Max() = %d, want 0 (negative clamped)", b.Max())
	}
	if ok := b.Consume(); ok {
		t.Errorf("Consume on clamped max=0 must return false")
	}
}
