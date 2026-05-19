package fsops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// --- apply_code_patch ------------------------------------------------------

func TestFSOps_ApplyCodePatch_HappyPath(t *testing.T) {
	e, dir := newEngine(t)
	original := "alpha\nbeta gamma\ndelta\nepsilon\n"
	if err := os.WriteFile(filepath.Join(dir, "x.txt"), []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	ch, _ := e.Run(context.Background(), Call{
		Tool: ToolApplyCodePatch,
		Args: map[string]any{
			"file_path":      "x.txt",
			"search_string":  "beta gamma",
			"replace_string": "BETA GAMMA",
		},
	}, Limits{})
	out, exit := collect(t, ch)
	if exit.ExitCode != 0 {
		t.Fatalf("exit = %+v", exit)
	}
	var r ApplyCodePatchResult
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatalf("json: %v", err)
	}
	if r.LineNumber != 2 {
		t.Errorf("line number = %d, want 2", r.LineNumber)
	}
	if r.Path != "x.txt" {
		t.Errorf("path = %q", r.Path)
	}
	if !strings.Contains(r.UnifiedDiff, "-beta gamma") || !strings.Contains(r.UnifiedDiff, "+BETA GAMMA") {
		t.Errorf("unified_diff missing +/- lines: %q", r.UnifiedDiff)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "x.txt"))
	if string(got) != "alpha\nBETA GAMMA\ndelta\nepsilon\n" {
		t.Errorf("file contents = %q", got)
	}
}

func TestFSOps_ApplyCodePatch_NoMatch(t *testing.T) {
	e, dir := newEngine(t)
	original := "alpha\nbeta\n"
	_ = os.WriteFile(filepath.Join(dir, "x.txt"), []byte(original), 0o644)
	ch, _ := e.Run(context.Background(), Call{
		Tool: ToolApplyCodePatch,
		Args: map[string]any{
			"file_path":      "x.txt",
			"search_string":  "missing",
			"replace_string": "nope",
		},
	}, Limits{})
	_, exit := collect(t, ch)
	if !errors.Is(exit.Err, sandbox.ErrBadRequest) {
		t.Fatalf("expected ErrBadRequest, got %v", exit.Err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "x.txt"))
	if string(got) != original {
		t.Errorf("file mutated despite error: %q", got)
	}
}

func TestFSOps_ApplyCodePatch_MultipleMatches(t *testing.T) {
	e, dir := newEngine(t)
	original := "x\nfoo\ny\nfoo\nz\n"
	_ = os.WriteFile(filepath.Join(dir, "x.txt"), []byte(original), 0o644)
	ch, _ := e.Run(context.Background(), Call{
		Tool: ToolApplyCodePatch,
		Args: map[string]any{
			"file_path":      "x.txt",
			"search_string":  "foo",
			"replace_string": "bar",
		},
	}, Limits{})
	_, exit := collect(t, ch)
	if !errors.Is(exit.Err, sandbox.ErrBadRequest) {
		t.Fatalf("expected ErrBadRequest, got %v", exit.Err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "x.txt"))
	if string(got) != original {
		t.Errorf("file mutated despite error: %q", got)
	}
}

func TestFSOps_ApplyCodePatch_RejectEscape_DotDot(t *testing.T) {
	e, _ := newEngine(t)
	ch, _ := e.Run(context.Background(), Call{
		Tool: ToolApplyCodePatch,
		Args: map[string]any{
			"file_path":      "../etc/passwd",
			"search_string":  "anything",
			"replace_string": "x",
		},
	}, Limits{})
	_, exit := collect(t, ch)
	if !errors.Is(exit.Err, sandbox.ErrBadRequest) {
		t.Fatalf("expected ErrBadRequest, got %v", exit.Err)
	}
}

func TestFSOps_ApplyCodePatch_EmptyReplace(t *testing.T) {
	e, dir := newEngine(t)
	_ = os.WriteFile(filepath.Join(dir, "x.txt"), []byte("keep [REMOVE] tail"), 0o644)
	ch, _ := e.Run(context.Background(), Call{
		Tool: ToolApplyCodePatch,
		Args: map[string]any{
			"file_path":      "x.txt",
			"search_string":  "[REMOVE] ",
			"replace_string": "",
		},
	}, Limits{})
	_, exit := collect(t, ch)
	if exit.ExitCode != 0 {
		t.Fatalf("exit = %+v", exit)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "x.txt"))
	if string(got) != "keep tail" {
		t.Errorf("file contents = %q", got)
	}
}

func TestFSOps_ApplyCodePatch_RejectMissingFile(t *testing.T) {
	e, _ := newEngine(t)
	ch, _ := e.Run(context.Background(), Call{
		Tool: ToolApplyCodePatch,
		Args: map[string]any{
			"file_path":      "nope.txt",
			"search_string":  "x",
			"replace_string": "y",
		},
	}, Limits{})
	_, exit := collect(t, ch)
	if !errors.Is(exit.Err, sandbox.ErrBadRequest) {
		t.Fatalf("expected ErrBadRequest, got %v", exit.Err)
	}
}

func TestFSOps_ApplyCodePatchWithSnapshot_RoundTrip(t *testing.T) {
	e, dir := newEngine(t)
	original := "alpha\nbeta gamma\ndelta\n"
	target := filepath.Join(dir, "x.txt")
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot, resolved, result, err := e.ApplyCodePatchWithSnapshot(context.Background(), map[string]any{
		"file_path":      "x.txt",
		"search_string":  "beta gamma",
		"replace_string": "BETA GAMMA",
	}, Limits{})
	if err != nil {
		t.Fatalf("ApplyCodePatchWithSnapshot: %v", err)
	}
	if string(snapshot) != original {
		t.Errorf("snapshot = %q, want %q", snapshot, original)
	}
	if result.LineNumber != 2 {
		t.Errorf("line number = %d, want 2", result.LineNumber)
	}
	if !strings.HasPrefix(resolved, dir) {
		t.Errorf("resolved path %q is not under %q", resolved, dir)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "alpha\nBETA GAMMA\ndelta\n" {
		t.Errorf("post-apply contents = %q", got)
	}
	// Restore must put the file back byte-for-byte.
	if err := e.RestoreFile(context.Background(), resolved, snapshot); err != nil {
		t.Fatalf("RestoreFile: %v", err)
	}
	got, _ = os.ReadFile(target)
	if string(got) != original {
		t.Errorf("post-restore contents = %q, want %q", got, original)
	}
}

func TestFSOps_RestoreFile_RejectOutsideRoot(t *testing.T) {
	e, _ := newEngine(t)
	// A second tempdir, used to forge a path outside the engine's root.
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "evil.txt")
	if err := os.WriteFile(outsideFile, []byte("intact"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := e.RestoreFile(context.Background(), outsideFile, []byte("hijacked"))
	if !errors.Is(err, ErrPathEscape) {
		t.Fatalf("want ErrPathEscape, got %v", err)
	}
	// File outside the engine's scope must not have been touched.
	got, _ := os.ReadFile(outsideFile)
	if string(got) != "intact" {
		t.Errorf("RestoreFile mutated out-of-scope file: %q", got)
	}
}

func TestFSOps_PreviewApplyCodePatch_NoWrite(t *testing.T) {
	e, dir := newEngine(t)
	original := "alpha\nbeta\n"
	target := filepath.Join(dir, "x.txt")
	_ = os.WriteFile(target, []byte(original), 0o644)
	before, _ := os.Stat(target)

	pv, err := e.PreviewApplyCodePatch(context.Background(), map[string]any{
		"file_path":      "x.txt",
		"search_string":  "beta",
		"replace_string": "BETA",
	})
	if err != nil {
		t.Fatalf("PreviewApplyCodePatch: %v", err)
	}
	if pv.LineNumber != 2 {
		t.Errorf("preview line = %d, want 2", pv.LineNumber)
	}
	if !strings.Contains(pv.UnifiedDiff, "-beta") || !strings.Contains(pv.UnifiedDiff, "+BETA") {
		t.Errorf("preview unified_diff missing +/- lines: %q", pv.UnifiedDiff)
	}

	// File untouched.
	got, _ := os.ReadFile(target)
	if string(got) != original {
		t.Errorf("preview wrote to file: %q", got)
	}
	after, _ := os.Stat(target)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("preview changed mtime: %v -> %v", before.ModTime(), after.ModTime())
	}
}

func TestFSOps_PreviewApplyCodePatch_PropagatesValidationErrors(t *testing.T) {
	e, dir := newEngine(t)
	_ = os.WriteFile(filepath.Join(dir, "x.txt"), []byte("foo foo"), 0o644)
	_, err := e.PreviewApplyCodePatch(context.Background(), map[string]any{
		"file_path":      "x.txt",
		"search_string":  "foo",
		"replace_string": "bar",
	})
	if !errors.Is(err, sandbox.ErrBadRequest) {
		t.Fatalf("expected ErrBadRequest, got %v", err)
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
