package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mattcheramie/nomaddev/internal/fsops"
	"github.com/mattcheramie/nomaddev/internal/sandbox"
)

// collectVerifyChunks drains the dispatcher's ExecChunk channel, returning
// concatenated stdout, stderr, and the terminal exit chunk separately so
// each assertion in the verify_command tests can pin down the part it
// cares about without re-parsing one stream.
func collectVerifyChunks(t *testing.T, ch <-chan sandbox.ExecChunk) (stdout, stderr bytes.Buffer, exit sandbox.ExecChunk) {
	t.Helper()
	for c := range ch {
		switch c.Stream {
		case sandbox.StreamStdout:
			stdout.Write(c.Data)
		case sandbox.StreamStderr:
			stderr.Write(c.Data)
		case sandbox.StreamExit:
			exit = c
		}
	}
	return
}

// newApplyVerifyHarness sets up a dispatcher with a real fsops engine
// rooted in a fresh tempdir plus a swappable mock sandbox runner. Returns
// the dispatcher, the workspace dir, and the seeded file path so tests
// can assert on post-run file contents.
func newApplyVerifyHarness(t *testing.T, sandboxScript []sandbox.ExecChunk, original string) (*CompositeDispatcher, string, string) {
	t.Helper()
	dir := t.TempDir()
	target := filepath.Join(dir, "x.txt")
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	fs, err := fsops.New(dir)
	if err != nil {
		t.Fatalf("fsops.New: %v", err)
	}
	mock := sandbox.NewMockRunner(sandboxScript...)
	return NewCompositeDispatcher(mock, fs), dir, target
}

func TestDispatcher_ApplyVerify_Success(t *testing.T) {
	disp, _, target := newApplyVerifyHarness(t,
		sandbox.MockScript("build ok\n", "", 0),
		"alpha\nbeta\n",
	)
	ch, err := disp.Dispatch(context.Background(), ToolCall{
		Tool: ToolApplyCodePatch,
		Args: map[string]any{
			"file_path":      "x.txt",
			"search_string":  "beta",
			"replace_string": "BETA",
			"verify_command": "go build ./...",
		},
	}, DispatchOptions{})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	stdout, stderr, exit := collectVerifyChunks(t, ch)
	if exit.ExitCode != 0 || exit.Err != nil {
		t.Fatalf("exit = %+v, want clean 0", exit)
	}
	// File should be left in the verified-good state.
	got, _ := os.ReadFile(target)
	if string(got) != "alpha\nBETA\n" {
		t.Errorf("file contents = %q, want %q", got, "alpha\nBETA\n")
	}
	// Stdout must include both the apply_code_patch JSON envelope and the
	// verify command's own stdout so the operator sees both events.
	if !strings.Contains(stdout.String(), "build ok") {
		t.Errorf("stdout missing verify output: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), `"line_number":2`) {
		t.Errorf("stdout missing apply_code_patch JSON: %q", stdout.String())
	}
	// No rollback notification on the success path.
	if strings.Contains(stderr.String(), "rolled back") {
		t.Errorf("unexpected rollback notification on success: %q", stderr.String())
	}
}

func TestDispatcher_ApplyVerify_RollbackOnNonZeroExit(t *testing.T) {
	original := "alpha\nbeta\n"
	disp, _, target := newApplyVerifyHarness(t,
		sandbox.MockScript("", "x.go:1:1: undefined: foo\n", 2),
		original,
	)
	ch, err := disp.Dispatch(context.Background(), ToolCall{
		Tool: ToolApplyCodePatch,
		Args: map[string]any{
			"file_path":      "x.txt",
			"search_string":  "beta",
			"replace_string": "BETA",
			"verify_command": "go build ./...",
		},
	}, DispatchOptions{})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	_, stderr, exit := collectVerifyChunks(t, ch)

	// Recovery-loop expectation: exit_code non-zero, errCode empty (no
	// SandboxErr*) so ShouldAutoRetry returns true and the LLM gets a
	// chance to fix the underlying issue.
	if exit.ExitCode != 2 {
		t.Errorf("exit code = %d, want 2 (verify command's exit)", exit.ExitCode)
	}
	if exit.Err != nil {
		t.Errorf("exit.Err = %v, want nil (clean non-zero shell exit)", exit.Err)
	}

	// stderr must carry both the verify command's own error output AND a
	// rollback notification so the recovery report stitches them into the
	// translator's next prompt with full context.
	if !strings.Contains(stderr.String(), "undefined: foo") {
		t.Errorf("stderr missing verify-command stderr: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "rolled back") {
		t.Errorf("stderr missing rollback notification: %q", stderr.String())
	}

	// And — the actual point of this whole feature — the file must be back
	// to its original contents.
	got, _ := os.ReadFile(target)
	if string(got) != original {
		t.Errorf("file contents = %q, want original %q (rollback failed)", got, original)
	}
}

func TestDispatcher_ApplyVerify_RollbackOnDispatchError(t *testing.T) {
	original := "alpha\nbeta\n"
	dir := t.TempDir()
	target := filepath.Join(dir, "x.txt")
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	fs, err := fsops.New(dir)
	if err != nil {
		t.Fatalf("fsops.New: %v", err)
	}
	boom := errors.New("docker daemon unavailable")
	// FailExec triggers an error from Sandbox.Exec before any chunks flow.
	mock := &sandbox.MockRunner{FailExec: boom}
	disp := NewCompositeDispatcher(mock, fs)

	ch, err := disp.Dispatch(context.Background(), ToolCall{
		Tool: ToolApplyCodePatch,
		Args: map[string]any{
			"file_path":      "x.txt",
			"search_string":  "beta",
			"replace_string": "BETA",
			"verify_command": "go build ./...",
		},
	}, DispatchOptions{})
	if err != nil {
		t.Fatalf("Dispatch returned error before streaming: %v", err)
	}
	_, stderr, exit := collectVerifyChunks(t, ch)

	// Dispatch errors flow through ExecChunk.Err so wsserver maps them
	// to SandboxErrInternal/SandboxErrBadRequest at the wire layer
	// (non-retryable — the LLM can't fix a runner outage).
	if !errors.Is(exit.Err, boom) {
		t.Errorf("exit.Err = %v, want %v", exit.Err, boom)
	}
	if !strings.Contains(stderr.String(), "rolled back") {
		t.Errorf("stderr missing rollback notification on dispatch error: %q", stderr.String())
	}
	got, _ := os.ReadFile(target)
	if string(got) != original {
		t.Errorf("file contents = %q, want %q (rollback failed)", got, original)
	}
}

func TestDispatcher_ApplyVerify_RequiresSandbox(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "x.txt")
	_ = os.WriteFile(target, []byte("alpha\n"), 0o644)
	fs, err := fsops.New(dir)
	if err != nil {
		t.Fatalf("fsops.New: %v", err)
	}
	// No sandbox runner — apply with verify must fast-fail at dispatch time.
	disp := NewCompositeDispatcher(nil, fs)
	_, err = disp.Dispatch(context.Background(), ToolCall{
		Tool: ToolApplyCodePatch,
		Args: map[string]any{
			"file_path":      "x.txt",
			"search_string":  "alpha",
			"replace_string": "ALPHA",
			"verify_command": "go build ./...",
		},
	}, DispatchOptions{})
	if !errors.Is(err, sandbox.ErrBadRequest) {
		t.Fatalf("want ErrBadRequest, got %v", err)
	}
	// Patch must NOT have been applied.
	got, _ := os.ReadFile(target)
	if string(got) != "alpha\n" {
		t.Errorf("file mutated despite fast-fail: %q", got)
	}
}

func TestDispatcher_ApplyVerify_EmptyVerifyCommandUsesPlainPath(t *testing.T) {
	// An empty verify_command is the same as omitting it: skip the
	// composition path and use plain fsops.Run. This keeps callers that
	// thread an unused field (e.g. UI defaults) from suddenly requiring
	// a sandbox runner.
	dir := t.TempDir()
	target := filepath.Join(dir, "x.txt")
	_ = os.WriteFile(target, []byte("alpha\n"), 0o644)
	fs, err := fsops.New(dir)
	if err != nil {
		t.Fatalf("fsops.New: %v", err)
	}
	disp := NewCompositeDispatcher(nil, fs)
	ch, err := disp.Dispatch(context.Background(), ToolCall{
		Tool: ToolApplyCodePatch,
		Args: map[string]any{
			"file_path":      "x.txt",
			"search_string":  "alpha",
			"replace_string": "ALPHA",
			"verify_command": "",
		},
	}, DispatchOptions{})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	stdout, _, exit := collectVerifyChunks(t, ch)
	if exit.ExitCode != 0 {
		t.Fatalf("exit = %+v", exit)
	}
	// The plain path emits the apply_code_patch JSON envelope and nothing else.
	var r fsops.ApplyCodePatchResult
	if jerr := json.Unmarshal(stdout.Bytes(), &r); jerr != nil {
		t.Fatalf("plain path stdout is not a single JSON envelope: %v / %q", jerr, stdout.String())
	}
	if r.Path != "x.txt" || r.LineNumber != 1 {
		t.Errorf("apply result = %+v", r)
	}
}
