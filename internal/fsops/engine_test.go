package fsops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mattcheramie/nomaddev/internal/sandbox"
)

func newEngine(t *testing.T) (*Engine, string) {
	t.Helper()
	dir := t.TempDir()
	e, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e, dir
}

func collect(t *testing.T, ch <-chan sandbox.ExecChunk) (stdout bytes.Buffer, exit sandbox.ExecChunk) {
	t.Helper()
	for c := range ch {
		switch c.Stream {
		case sandbox.StreamStdout:
			stdout.Write(c.Data)
		case sandbox.StreamExit:
			exit = c
		}
	}
	return
}

// --- read_file -------------------------------------------------------------

func TestFSOps_ReadFile_Happy(t *testing.T) {
	e, dir := newEngine(t)
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ch, err := e.Run(context.Background(), Call{Tool: ToolReadFile, Args: map[string]any{"path": "note.txt"}}, Limits{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	out, exit := collect(t, ch)
	if exit.ExitCode != 0 {
		t.Fatalf("exit = %+v", exit)
	}
	if out.String() != "hello\n" {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestFSOps_ReadFile_RespectsMaxBytes(t *testing.T) {
	e, dir := newEngine(t)
	_ = os.WriteFile(filepath.Join(dir, "big.txt"), []byte("abcdefghij"), 0o644)
	ch, _ := e.Run(context.Background(), Call{
		Tool: ToolReadFile,
		Args: map[string]any{"path": "big.txt", "max_bytes": 3},
	}, Limits{})
	out, exit := collect(t, ch)
	if exit.ExitCode != 0 {
		t.Fatalf("exit = %+v", exit)
	}
	if out.String() != "abc" {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestFSOps_ReadFile_RejectEscape_DotDot(t *testing.T) {
	e, _ := newEngine(t)
	ch, _ := e.Run(context.Background(), Call{
		Tool: ToolReadFile,
		Args: map[string]any{"path": "../etc/passwd"},
	}, Limits{})
	_, exit := collect(t, ch)
	if !errors.Is(exit.Err, sandbox.ErrBadRequest) {
		t.Fatalf("expected ErrBadRequest, got %v", exit.Err)
	}
}

func TestFSOps_ReadFile_RejectAbsolute(t *testing.T) {
	e, _ := newEngine(t)
	ch, _ := e.Run(context.Background(), Call{
		Tool: ToolReadFile,
		Args: map[string]any{"path": "/etc/passwd"},
	}, Limits{})
	_, exit := collect(t, ch)
	if !errors.Is(exit.Err, sandbox.ErrBadRequest) {
		t.Fatalf("expected ErrBadRequest, got %v", exit.Err)
	}
}

func TestFSOps_ReadFile_RejectSymlinkOutsideRoot(t *testing.T) {
	e, dir := newEngine(t)
	// Create a target file outside the workspace and a symlink inside it.
	outside := t.TempDir()
	target := filepath.Join(outside, "secret.txt")
	_ = os.WriteFile(target, []byte("nope"), 0o644)
	if err := os.Symlink(target, filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	ch, _ := e.Run(context.Background(), Call{
		Tool: ToolReadFile,
		Args: map[string]any{"path": "link"},
	}, Limits{})
	_, exit := collect(t, ch)
	if !errors.Is(exit.Err, sandbox.ErrBadRequest) {
		t.Fatalf("expected ErrBadRequest, got %v", exit.Err)
	}
}

func TestFSOps_ReadFile_RejectMissingPath(t *testing.T) {
	e, _ := newEngine(t)
	ch, _ := e.Run(context.Background(), Call{Tool: ToolReadFile, Args: map[string]any{}}, Limits{})
	_, exit := collect(t, ch)
	if !errors.Is(exit.Err, sandbox.ErrBadRequest) {
		t.Fatalf("expected ErrBadRequest, got %v", exit.Err)
	}
}

func TestFSOps_ReadFile_RejectDirectory(t *testing.T) {
	e, dir := newEngine(t)
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	ch, _ := e.Run(context.Background(), Call{
		Tool: ToolReadFile,
		Args: map[string]any{"path": "sub"},
	}, Limits{})
	_, exit := collect(t, ch)
	if !errors.Is(exit.Err, sandbox.ErrBadRequest) {
		t.Fatalf("expected ErrBadRequest, got %v", exit.Err)
	}
}

// --- list_dir --------------------------------------------------------------

func TestFSOps_ListDir_Flat(t *testing.T) {
	e, dir := newEngine(t)
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0o644)
	_ = os.Mkdir(filepath.Join(dir, "b"), 0o755)
	ch, _ := e.Run(context.Background(), Call{Tool: ToolListDir, Args: map[string]any{"path": "."}}, Limits{})
	out, exit := collect(t, ch)
	if exit.ExitCode != 0 {
		t.Fatalf("exit = %+v", exit)
	}
	var r listResult
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatalf("json: %v", err)
	}
	if len(r.Entries) != 2 {
		t.Fatalf("want 2 entries, got %d: %+v", len(r.Entries), r.Entries)
	}
	names := map[string]string{}
	for _, en := range r.Entries {
		names[en.Name] = en.Type
	}
	if names["a.txt"] != "file" || names["b"] != "dir" {
		t.Errorf("types wrong: %+v", names)
	}
}

func TestFSOps_ListDir_DepthLimit(t *testing.T) {
	e, dir := newEngine(t)
	_ = os.MkdirAll(filepath.Join(dir, "a", "b", "c"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "a", "leaf.txt"), []byte(""), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "a", "b", "deep.txt"), []byte(""), 0o644)

	// depth=1: only direct children of root.
	ch1, _ := e.Run(context.Background(), Call{
		Tool: ToolListDir, Args: map[string]any{"path": ".", "depth": 1},
	}, Limits{})
	out1, exit1 := collect(t, ch1)
	if exit1.ExitCode != 0 {
		t.Fatalf("depth=1 exit = %+v", exit1)
	}
	var r1 listResult
	_ = json.Unmarshal(out1.Bytes(), &r1)
	if len(r1.Entries) != 1 || r1.Entries[0].Name != "a" {
		t.Errorf("depth=1 entries = %+v", r1.Entries)
	}

	// depth=2: children + grandchildren. Includes a/leaf.txt and a/b but NOT a/b/deep.txt.
	ch2, _ := e.Run(context.Background(), Call{
		Tool: ToolListDir, Args: map[string]any{"path": ".", "depth": 2},
	}, Limits{})
	out2, _ := collect(t, ch2)
	var r2 listResult
	_ = json.Unmarshal(out2.Bytes(), &r2)
	names := map[string]bool{}
	for _, en := range r2.Entries {
		names[en.Name] = true
	}
	if !names["a"] || !names["a/leaf.txt"] || !names["a/b"] {
		t.Errorf("depth=2 missing entries; got %+v", r2.Entries)
	}
	if names["a/b/deep.txt"] || names["a/b/c"] {
		t.Errorf("depth=2 should NOT recurse to level 3; got %+v", r2.Entries)
	}

	// depth=3: includes a/b/deep.txt and a/b/c.
	ch3, _ := e.Run(context.Background(), Call{
		Tool: ToolListDir, Args: map[string]any{"path": ".", "depth": 3},
	}, Limits{})
	out3, _ := collect(t, ch3)
	var r3 listResult
	_ = json.Unmarshal(out3.Bytes(), &r3)
	hasDeep := false
	for _, en := range r3.Entries {
		if en.Name == "a/b/deep.txt" {
			hasDeep = true
		}
	}
	if !hasDeep {
		t.Errorf("depth=3 should reveal a/b/deep.txt; entries=%+v", r3.Entries)
	}
}

// --- write_patch -----------------------------------------------------------

func TestFSOps_WritePatch_Create(t *testing.T) {
	e, dir := newEngine(t)
	ch, _ := e.Run(context.Background(), Call{
		Tool: ToolWritePatch,
		Args: map[string]any{"path": "new.txt", "content": "hi", "create": true},
	}, Limits{})
	out, exit := collect(t, ch)
	if exit.ExitCode != 0 {
		t.Fatalf("exit = %+v", exit)
	}
	var r writeResult
	_ = json.Unmarshal(out.Bytes(), &r)
	if r.BytesWritten != 2 || r.Path != "new.txt" {
		t.Errorf("result = %+v", r)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "new.txt"))
	if string(got) != "hi" {
		t.Errorf("file contents = %q", got)
	}
}

func TestFSOps_WritePatch_Overwrite(t *testing.T) {
	e, dir := newEngine(t)
	_ = os.WriteFile(filepath.Join(dir, "x.txt"), []byte("old"), 0o644)
	ch, _ := e.Run(context.Background(), Call{
		Tool: ToolWritePatch,
		Args: map[string]any{"path": "x.txt", "content": "new"},
	}, Limits{})
	_, exit := collect(t, ch)
	if exit.ExitCode != 0 {
		t.Fatalf("exit = %+v", exit)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "x.txt"))
	if string(got) != "new" {
		t.Errorf("file contents = %q", got)
	}
}

func TestFSOps_WritePatch_Append(t *testing.T) {
	e, dir := newEngine(t)
	_ = os.WriteFile(filepath.Join(dir, "log.txt"), []byte("one\n"), 0o644)
	ch, _ := e.Run(context.Background(), Call{
		Tool: ToolWritePatch,
		Args: map[string]any{"path": "log.txt", "content": "two\n", "mode": "append"},
	}, Limits{})
	_, exit := collect(t, ch)
	if exit.ExitCode != 0 {
		t.Fatalf("exit = %+v", exit)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "log.txt"))
	if string(got) != "one\ntwo\n" {
		t.Errorf("file contents = %q", got)
	}
}

func TestFSOps_WritePatch_RejectParentEscape(t *testing.T) {
	e, _ := newEngine(t)
	ch, _ := e.Run(context.Background(), Call{
		Tool: ToolWritePatch,
		Args: map[string]any{"path": "../escape.txt", "content": "no"},
	}, Limits{})
	_, exit := collect(t, ch)
	if !errors.Is(exit.Err, sandbox.ErrBadRequest) {
		t.Fatalf("expected ErrBadRequest, got %v", exit.Err)
	}
}

func TestFSOps_WritePatch_RejectInvalidMode(t *testing.T) {
	e, _ := newEngine(t)
	ch, _ := e.Run(context.Background(), Call{
		Tool: ToolWritePatch,
		Args: map[string]any{"path": "x.txt", "content": "hi", "mode": "rewrite"},
	}, Limits{})
	_, exit := collect(t, ch)
	if !errors.Is(exit.Err, sandbox.ErrBadRequest) {
		t.Fatalf("expected ErrBadRequest, got %v", exit.Err)
	}
}

// --- contract --------------------------------------------------------------

func TestFSOps_ExecChunkContract_ChannelClosedExactlyOnce(t *testing.T) {
	e, dir := newEngine(t)
	_ = os.WriteFile(filepath.Join(dir, "x"), []byte("x"), 0o644)
	ch, _ := e.Run(context.Background(), Call{Tool: ToolReadFile, Args: map[string]any{"path": "x"}}, Limits{})
	count := 0
	var last sandbox.ExecChunk
	for c := range ch {
		count++
		last = c
	}
	if last.Stream != sandbox.StreamExit {
		t.Errorf("final chunk = %+v, want exit", last)
	}
	_, ok := <-ch
	if ok {
		t.Fatalf("channel still open after drain")
	}
}

func TestFSOps_UnknownTool(t *testing.T) {
	e, _ := newEngine(t)
	_, err := e.Run(context.Background(), Call{Tool: "nope"}, Limits{})
	if !errors.Is(err, sandbox.ErrBadRequest) {
		t.Fatalf("expected ErrBadRequest, got %v", err)
	}
}
