package middleware

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mattcheramie/nomaddev/internal/fsops"
	"github.com/mattcheramie/nomaddev/internal/history"
	"github.com/mattcheramie/nomaddev/internal/sandbox"
)

// newPinsHarness builds a dispatcher with a real fsops engine rooted in a
// fresh tempdir and an attached reference buffer. Returns the dispatcher
// and the workspace dir so tests can rewrite seeded files.
func newPinsHarness(t *testing.T, seed map[string]string) (*CompositeDispatcher, string) {
	t.Helper()
	dir := t.TempDir()
	for name, content := range seed {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fs, err := fsops.New(dir)
	if err != nil {
		t.Fatalf("fsops.New: %v", err)
	}
	d := NewCompositeDispatcher(nil, fs)
	d.Pins = history.NewReferenceBuffer()
	return d, dir
}

func TestDispatcher_PinFile_Success(t *testing.T) {
	d, _ := newPinsHarness(t, map[string]string{"core.go": "package core\n// data structures\n"})
	ch, err := d.Dispatch(context.Background(),
		ToolCall{Tool: ToolPinFile, Args: map[string]any{"path": "core.go"}},
		DispatchOptions{SessionID: "s1"})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	stdout, _, exit := collectVerifyChunks(t, ch)
	if exit.ExitCode != 0 || exit.Err != nil {
		t.Fatalf("exit = %+v, want clean 0", exit)
	}
	if !strings.Contains(stdout.String(), "pinned") {
		t.Errorf("missing confirmation in stdout: %q", stdout.String())
	}
	rendered := d.Pins.Render("s1")
	if !strings.Contains(rendered, "core.go") || !strings.Contains(rendered, "package core") {
		t.Errorf("buffer does not carry the pinned contents: %q", rendered)
	}
}

func TestDispatcher_PinFile_MissingFile(t *testing.T) {
	d, _ := newPinsHarness(t, nil)
	ch, err := d.Dispatch(context.Background(),
		ToolCall{Tool: ToolPinFile, Args: map[string]any{"path": "ghost.go"}},
		DispatchOptions{SessionID: "s1"})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	_, _, exit := collectVerifyChunks(t, ch)
	if exit.ExitCode == 0 || exit.Err == nil {
		t.Fatalf("want a failure exit for a missing file, got %+v", exit)
	}
	if got := d.Pins.Render("s1"); got != "" {
		t.Errorf("a failed pin left state in the buffer: %q", got)
	}
}

func TestDispatcher_PinFile_RejectsPathEscape(t *testing.T) {
	d, _ := newPinsHarness(t, nil)
	ch, err := d.Dispatch(context.Background(),
		ToolCall{Tool: ToolPinFile, Args: map[string]any{"path": "../etc/passwd"}},
		DispatchOptions{SessionID: "s1"})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	_, _, exit := collectVerifyChunks(t, ch)
	if exit.Err == nil {
		t.Fatal("path-escape pin should fail")
	}
	if got := d.Pins.Render("s1"); got != "" {
		t.Errorf("path-escape pin left state in the buffer: %q", got)
	}
}

func TestDispatcher_PinFile_NoBufferConfigured(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "core.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs, err := fsops.New(dir)
	if err != nil {
		t.Fatalf("fsops.New: %v", err)
	}
	d := NewCompositeDispatcher(nil, fs) // .Pins left nil
	_, err = d.Dispatch(context.Background(),
		ToolCall{Tool: ToolPinFile, Args: map[string]any{"path": "core.go"}},
		DispatchOptions{SessionID: "s1"})
	if !errors.Is(err, sandbox.ErrBadRequest) {
		t.Fatalf("want ErrBadRequest when buffer unconfigured, got %v", err)
	}
}

func TestDispatcher_PinFile_NoFSOps(t *testing.T) {
	d := NewCompositeDispatcher(nil, nil)
	d.Pins = history.NewReferenceBuffer()
	_, err := d.Dispatch(context.Background(),
		ToolCall{Tool: ToolPinFile, Args: map[string]any{"path": "core.go"}},
		DispatchOptions{SessionID: "s1"})
	if !errors.Is(err, sandbox.ErrBadRequest) {
		t.Fatalf("want ErrBadRequest when fsops unconfigured, got %v", err)
	}
}

func TestDispatcher_PinFile_AllowedInAuditMode(t *testing.T) {
	d, _ := newPinsHarness(t, map[string]string{"core.go": "package core\n"})
	ch, err := d.Dispatch(context.Background(),
		ToolCall{Tool: ToolPinFile, Args: map[string]any{"path": "core.go"}},
		DispatchOptions{SessionID: "s1", Mode: ModeAudit})
	if err != nil {
		t.Fatalf("pin_file rejected in audit mode: %v", err)
	}
	_, _, exit := collectVerifyChunks(t, ch)
	if exit.ExitCode != 0 {
		t.Fatalf("pin_file failed in audit mode: %+v", exit)
	}
}

func TestDispatcher_PinFile_RePinRefreshes(t *testing.T) {
	d, dir := newPinsHarness(t, map[string]string{"core.go": "version one\n"})
	pin := func() {
		ch, err := d.Dispatch(context.Background(),
			ToolCall{Tool: ToolPinFile, Args: map[string]any{"path": "core.go"}},
			DispatchOptions{SessionID: "s1"})
		if err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		_, _, exit := collectVerifyChunks(t, ch)
		if exit.ExitCode != 0 {
			t.Fatalf("exit = %+v", exit)
		}
	}
	pin()
	if err := os.WriteFile(filepath.Join(dir, "core.go"), []byte("version two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pin()
	rendered := d.Pins.Render("s1")
	if strings.Contains(rendered, "version one") || !strings.Contains(rendered, "version two") {
		t.Errorf("re-pin did not refresh the buffer: %q", rendered)
	}
}

func TestDispatcher_UnpinFile_Success(t *testing.T) {
	d, _ := newPinsHarness(t, nil)
	if err := d.Pins.Pin("s1", "core.go", []byte("package core\n")); err != nil {
		t.Fatal(err)
	}
	ch, err := d.Dispatch(context.Background(),
		ToolCall{Tool: ToolUnpinFile, Args: map[string]any{"path": "core.go"}},
		DispatchOptions{SessionID: "s1"})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	stdout, _, exit := collectVerifyChunks(t, ch)
	if exit.ExitCode != 0 || exit.Err != nil {
		t.Fatalf("exit = %+v, want clean 0", exit)
	}
	if !strings.Contains(stdout.String(), "unpinned") {
		t.Errorf("missing confirmation in stdout: %q", stdout.String())
	}
	if got := d.Pins.Render("s1"); got != "" {
		t.Errorf("file still pinned after unpin: %q", got)
	}
}

func TestDispatcher_UnpinFile_NotPinned(t *testing.T) {
	d, _ := newPinsHarness(t, nil)
	ch, err := d.Dispatch(context.Background(),
		ToolCall{Tool: ToolUnpinFile, Args: map[string]any{"path": "never.go"}},
		DispatchOptions{SessionID: "s1"})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	stdout, _, exit := collectVerifyChunks(t, ch)
	// Unpinning an unpinned path is a clean no-op, not a failure.
	if exit.ExitCode != 0 || exit.Err != nil {
		t.Fatalf("want a clean exit for a no-op unpin, got %+v", exit)
	}
	if !strings.Contains(stdout.String(), "not pinned") {
		t.Errorf("missing no-op message in stdout: %q", stdout.String())
	}
}

func TestDispatcher_UnpinFile_NoBufferConfigured(t *testing.T) {
	d := NewCompositeDispatcher(nil, nil) // .Pins left nil
	_, err := d.Dispatch(context.Background(),
		ToolCall{Tool: ToolUnpinFile, Args: map[string]any{"path": "core.go"}},
		DispatchOptions{SessionID: "s1"})
	if !errors.Is(err, sandbox.ErrBadRequest) {
		t.Fatalf("want ErrBadRequest when buffer unconfigured, got %v", err)
	}
}
