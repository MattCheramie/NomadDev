package fsops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/mattcheramie/nomaddev/internal/sandbox"
)

// Tool names. These must match middleware.ToolReadFile / ToolListDir /
// ToolWritePatch — duplicated here to avoid an import cycle with the
// middleware package (which imports fsops).
const (
	ToolReadFile       = "read_file"
	ToolListDir        = "list_dir"
	ToolWritePatch     = "write_patch"
	ToolApplyCodePatch = "apply_code_patch"
)

// Call is a tool invocation passed to Engine.Run. Args are validated per-tool.
type Call struct {
	Tool string
	Args map[string]any
}

// Limits matches the per-call caps the middleware threads through. Only
// MaxBytes is meaningful for fsops today.
type Limits struct {
	ReadFileMaxBytes  int64 // 0 = use defaultReadFileMaxBytes
	WriteFileMaxBytes int64 // 0 = use defaultWriteFileMaxBytes
}

const (
	defaultReadFileMaxBytes  = 256 * 1024
	hardReadFileMaxBytes     = 4 * 1024 * 1024
	defaultWriteFileMaxBytes = 4 * 1024 * 1024
	maxListDepth             = 4
	defaultListDepth         = 1
	emitChunkBytes           = 16 * 1024
)

// Run dispatches one Call and returns a channel that emits sandbox.ExecChunk
// frames matching the sandbox.Runner contract: zero or more stdout/stderr
// chunks then exactly one Stream==StreamExit chunk, then closes.
func (e *Engine) Run(ctx context.Context, c Call, limits Limits) (<-chan sandbox.ExecChunk, error) {
	out := make(chan sandbox.ExecChunk, 8)
	switch c.Tool {
	case ToolReadFile:
		go e.runReadFile(ctx, c.Args, limits, out)
	case ToolListDir:
		go e.runListDir(ctx, c.Args, out)
	case ToolWritePatch:
		go e.runWritePatch(ctx, c.Args, limits, out)
	case ToolApplyCodePatch:
		go e.runApplyCodePatch(ctx, c.Args, limits, out)
	default:
		close(out)
		return nil, fmt.Errorf("%w: unknown fsops tool %q", sandbox.ErrBadRequest, c.Tool)
	}
	return out, nil
}

// argString extracts a non-empty string arg.
func argString(args map[string]any, key string) (string, error) {
	v, ok := args[key]
	if !ok {
		return "", fmt.Errorf("%w: missing %q", sandbox.ErrBadRequest, key)
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", fmt.Errorf("%w: %q must be non-empty string", sandbox.ErrBadRequest, key)
	}
	return s, nil
}

// argInt extracts an optional int-valued arg (JSON numbers decode as float64).
func argInt(args map[string]any, key string) (int, bool) {
	v, ok := args[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

// argBool extracts an optional bool-valued arg.
func argBool(args map[string]any, key string) (bool, bool) {
	v, ok := args[key]
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

// emitJSONResult marshals v and emits it as one or more stdout chunks
// (chunked at emitChunkBytes), then a clean exit.
func emitJSONResult(ctx context.Context, out chan<- sandbox.ExecChunk, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1, Err: err}
		return
	}
	emitBytesResult(ctx, out, body)
}

// emitBytesResult chunks data onto stdout and finishes with a clean exit.
func emitBytesResult(ctx context.Context, out chan<- sandbox.ExecChunk, data []byte) {
	for off := 0; off < len(data); off += emitChunkBytes {
		end := off + emitChunkBytes
		if end > len(data) {
			end = len(data)
		}
		select {
		case out <- sandbox.ExecChunk{Stream: sandbox.StreamStdout, Data: append([]byte(nil), data[off:end]...)}:
		case <-ctx.Done():
			out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1, Err: ctx.Err()}
			return
		}
	}
	out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: 0}
}

// --- read_file -------------------------------------------------------------

func (e *Engine) runReadFile(ctx context.Context, args map[string]any, limits Limits, out chan<- sandbox.ExecChunk) {
	defer close(out)
	path, err := argString(args, "path")
	if err != nil {
		out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1, Err: err}
		return
	}
	maxBytes := int64(defaultReadFileMaxBytes)
	if n, ok := argInt(args, "max_bytes"); ok && n > 0 {
		maxBytes = int64(n)
	}
	if limits.ReadFileMaxBytes > 0 && limits.ReadFileMaxBytes < maxBytes {
		maxBytes = limits.ReadFileMaxBytes
	}
	if maxBytes > hardReadFileMaxBytes {
		maxBytes = hardReadFileMaxBytes
	}

	resolved, err := e.resolveSafe(ctx, path)
	if err != nil {
		out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1,
			Err: fmt.Errorf("%w: %v", sandbox.ErrBadRequest, err)}
		return
	}

	f, err := os.Open(resolved)
	if err != nil {
		out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1,
			Err: fmt.Errorf("read_file open: %w", err)}
		return
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1, Err: err}
		return
	}
	if stat.IsDir() {
		out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1,
			Err: fmt.Errorf("%w: %q is a directory", sandbox.ErrBadRequest, path)}
		return
	}

	// Stream file contents in chunks bounded by maxBytes.
	buf := make([]byte, emitChunkBytes)
	var total int64
	for total < maxBytes {
		room := maxBytes - total
		if room < int64(len(buf)) {
			buf = buf[:room]
		}
		n, rerr := f.Read(buf)
		if n > 0 {
			select {
			case out <- sandbox.ExecChunk{Stream: sandbox.StreamStdout, Data: append([]byte(nil), buf[:n]...)}:
				total += int64(n)
			case <-ctx.Done():
				out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1, Err: ctx.Err()}
				return
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1, Err: rerr}
			return
		}
	}
	out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: 0}
}

// --- list_dir --------------------------------------------------------------

// listEntry mirrors the JSON shape the middleware returns to the model.
type listEntry struct {
	Name string `json:"name"`
	Type string `json:"type"` // "file"|"dir"|"symlink"|"other"
	Size int64  `json:"size,omitempty"`
}

type listResult struct {
	Path    string      `json:"path"`
	Entries []listEntry `json:"entries"`
}

func (e *Engine) runListDir(ctx context.Context, args map[string]any, out chan<- sandbox.ExecChunk) {
	defer close(out)
	path, err := argString(args, "path")
	if err != nil {
		out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1, Err: err}
		return
	}
	depth := defaultListDepth
	if n, ok := argInt(args, "depth"); ok && n > 0 {
		depth = n
	}
	if depth > maxListDepth {
		depth = maxListDepth
	}

	resolved, err := e.resolveSafe(ctx, path)
	if err != nil {
		out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1,
			Err: fmt.Errorf("%w: %v", sandbox.ErrBadRequest, err)}
		return
	}
	stat, err := os.Stat(resolved)
	if err != nil {
		out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1,
			Err: fmt.Errorf("list_dir stat: %w", err)}
		return
	}
	if !stat.IsDir() {
		out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1,
			Err: fmt.Errorf("%w: %q is not a directory", sandbox.ErrBadRequest, path)}
		return
	}

	var entries []listEntry
	walkErr := walkDepth(resolved, depth, func(rel string, info fs.DirEntry) error {
		t := entryType(info)
		var size int64
		if info.Type().IsRegular() {
			if fi, ferr := info.Info(); ferr == nil {
				size = fi.Size()
			}
		}
		entries = append(entries, listEntry{Name: rel, Type: t, Size: size})
		return nil
	})
	if walkErr != nil {
		out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1, Err: walkErr}
		return
	}
	emitJSONResult(ctx, out, listResult{Path: path, Entries: entries})
}

func entryType(d fs.DirEntry) string {
	switch {
	case d.IsDir():
		return "dir"
	case d.Type()&fs.ModeSymlink != 0:
		return "symlink"
	case d.Type().IsRegular():
		return "file"
	default:
		return "other"
	}
}

// walkDepth walks dir up to depth levels (depth=1 = direct children only).
// rel paths are reported as forward-slash relative names from dir.
func walkDepth(dir string, depth int, fn func(rel string, d fs.DirEntry) error) error {
	if depth <= 0 {
		return nil
	}
	children, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, c := range children {
		rel := c.Name()
		if err := fn(rel, c); err != nil {
			return err
		}
		if c.IsDir() && depth > 1 {
			if err := walkDepthInner(filepath.Join(dir, c.Name()), rel, depth-1, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

func walkDepthInner(dir, prefix string, depth int, fn func(rel string, d fs.DirEntry) error) error {
	if depth <= 0 {
		return nil
	}
	children, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, c := range children {
		rel := prefix + "/" + c.Name()
		if err := fn(rel, c); err != nil {
			return err
		}
		if c.IsDir() && depth > 1 {
			if err := walkDepthInner(filepath.Join(dir, c.Name()), rel, depth-1, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

// --- write_patch -----------------------------------------------------------

type writeResult struct {
	Path         string `json:"path"`
	BytesWritten int    `json:"bytes_written"`
	Mode         string `json:"mode"`
}

func (e *Engine) runWritePatch(ctx context.Context, args map[string]any, limits Limits, out chan<- sandbox.ExecChunk) {
	defer close(out)
	path, err := argString(args, "path")
	if err != nil {
		out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1, Err: err}
		return
	}
	content, err := argString(args, "content")
	if err != nil {
		out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1, Err: err}
		return
	}
	mode := "overwrite"
	if v, ok := args["mode"]; ok {
		if s, sok := v.(string); sok {
			mode = strings.ToLower(s)
		}
	}
	if mode != "overwrite" && mode != "append" {
		out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1,
			Err: fmt.Errorf("%w: mode must be 'overwrite' or 'append'", sandbox.ErrBadRequest)}
		return
	}
	create, _ := argBool(args, "create")

	maxBytes := int64(defaultWriteFileMaxBytes)
	if limits.WriteFileMaxBytes > 0 {
		maxBytes = limits.WriteFileMaxBytes
	}
	if int64(len(content)) > maxBytes {
		out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1,
			Err: fmt.Errorf("%w: content exceeds %d bytes", sandbox.ErrBadRequest, maxBytes)}
		return
	}

	resolved, err := e.resolveSafe(ctx, path)
	if err != nil {
		out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1,
			Err: fmt.Errorf("%w: %v", sandbox.ErrBadRequest, err)}
		return
	}

	if create {
		if mkErr := os.MkdirAll(filepath.Dir(resolved), 0o755); mkErr != nil {
			out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1, Err: mkErr}
			return
		}
	}

	var f *os.File
	if mode == "append" {
		f, err = os.OpenFile(resolved, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	} else {
		f, err = os.OpenFile(resolved, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	}
	if err != nil {
		out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1,
			Err: fmt.Errorf("write_patch open: %w", err)}
		return
	}
	defer f.Close()

	n, err := f.WriteString(content)
	if err != nil {
		out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1, Err: err}
		return
	}
	emitJSONResult(ctx, out, writeResult{Path: path, BytesWritten: n, Mode: mode})
}

// --- apply_code_patch ------------------------------------------------------

// ApplyCodePatchPreview is the dry-run result the approval pipeline surfaces
// in the mobile ApprovalSheet before any write happens. Produced by
// PreviewApplyCodePatch and exposed on the tool.approval.request envelope.
type ApplyCodePatchPreview struct {
	Path        string `json:"path"`
	LineNumber  int    `json:"line_number"`
	UnifiedDiff string `json:"unified_diff"`
}

type applyCodePatchResult struct {
	Path         string `json:"path"`
	BytesWritten int    `json:"bytes_written"`
	LineNumber   int    `json:"line_number"`
	UnifiedDiff  string `json:"unified_diff"`
}

// applyCodePatchPlan is the result of computeApplyCodePatch: it's everything
// needed to either preview or commit the edit. The runApplyCodePatch path
// then writes content; the PreviewApplyCodePatch path discards it.
type applyCodePatchPlan struct {
	resolved    string
	path        string
	newContent  string
	lineNumber  int
	unifiedDiff string
}

// PreviewApplyCodePatch runs the dry-run half of apply_code_patch. It reads
// the target, verifies the search anchor occurs exactly once, and renders a
// unified-diff preview — no write. Returned errors wrap sandbox.ErrBadRequest
// for validation failures so the approval pipeline can surface them as a
// fast-fail tool result.
func (e *Engine) PreviewApplyCodePatch(ctx context.Context, args map[string]any) (*ApplyCodePatchPreview, error) {
	plan, err := e.computeApplyCodePatch(ctx, args, Limits{})
	if err != nil {
		return nil, err
	}
	return &ApplyCodePatchPreview{
		Path:        plan.path,
		LineNumber:  plan.lineNumber,
		UnifiedDiff: plan.unifiedDiff,
	}, nil
}

func (e *Engine) runApplyCodePatch(ctx context.Context, args map[string]any, limits Limits, out chan<- sandbox.ExecChunk) {
	defer close(out)
	// Re-run the dry-run on the fresh file contents — closes the TOCTOU
	// window between the approval-time preview and the actual write.
	plan, err := e.computeApplyCodePatch(ctx, args, limits)
	if err != nil {
		out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1, Err: err}
		return
	}

	f, err := os.OpenFile(plan.resolved, os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1,
			Err: fmt.Errorf("apply_code_patch open: %w", err)}
		return
	}
	defer f.Close()

	n, err := f.WriteString(plan.newContent)
	if err != nil {
		out <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: -1, Err: err}
		return
	}
	emitJSONResult(ctx, out, applyCodePatchResult{
		Path:         plan.path,
		BytesWritten: n,
		LineNumber:   plan.lineNumber,
		UnifiedDiff:  plan.unifiedDiff,
	})
}

// computeApplyCodePatch performs the read-side work shared by preview and
// apply: arg extraction, path safety, single-match validation, replacement
// content, and the unified-diff render. No write side-effects.
func (e *Engine) computeApplyCodePatch(ctx context.Context, args map[string]any, limits Limits) (applyCodePatchPlan, error) {
	path, err := argString(args, "file_path")
	if err != nil {
		return applyCodePatchPlan{}, err
	}
	search, err := argString(args, "search_string")
	if err != nil {
		return applyCodePatchPlan{}, err
	}
	replace, err := argStringAllowEmpty(args, "replace_string")
	if err != nil {
		return applyCodePatchPlan{}, err
	}

	resolved, err := e.resolveSafe(ctx, path)
	if err != nil {
		return applyCodePatchPlan{}, fmt.Errorf("%w: %v", sandbox.ErrBadRequest, err)
	}
	raw, err := os.ReadFile(resolved)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return applyCodePatchPlan{}, fmt.Errorf("%w: file %q does not exist", sandbox.ErrBadRequest, path)
		}
		return applyCodePatchPlan{}, fmt.Errorf("apply_code_patch read: %w", err)
	}
	// resolveSafe accepts directories the same way write_patch does; reject
	// here so the error is specific.
	if stat, serr := os.Stat(resolved); serr == nil && stat.IsDir() {
		return applyCodePatchPlan{}, fmt.Errorf("%w: %q is a directory", sandbox.ErrBadRequest, path)
	}

	content := string(raw)
	count := strings.Count(content, search)
	switch {
	case count == 0:
		return applyCodePatchPlan{}, fmt.Errorf("%w: search_string not found in %q", sandbox.ErrBadRequest, path)
	case count > 1:
		return applyCodePatchPlan{}, fmt.Errorf("%w: search_string matches %d times in %q; must be unique", sandbox.ErrBadRequest, count, path)
	}

	idx := strings.Index(content, search)
	newContent := content[:idx] + replace + content[idx+len(search):]

	maxBytes := int64(defaultWriteFileMaxBytes)
	if limits.WriteFileMaxBytes > 0 {
		maxBytes = limits.WriteFileMaxBytes
	}
	if int64(len(newContent)) > maxBytes {
		return applyCodePatchPlan{}, fmt.Errorf("%w: result exceeds %d bytes", sandbox.ErrBadRequest, maxBytes)
	}

	line := 1 + strings.Count(content[:idx], "\n")
	diff := renderUnifiedDiff(path, content, newContent, idx, len(search), len(replace))

	return applyCodePatchPlan{
		resolved:    resolved,
		path:        path,
		newContent:  newContent,
		lineNumber:  line,
		unifiedDiff: diff,
	}, nil
}

// argStringAllowEmpty extracts a string arg that must be present and string-
// typed but is allowed to be the empty string. Used by apply_code_patch's
// replace_string (which can be "" for pure deletion).
func argStringAllowEmpty(args map[string]any, key string) (string, error) {
	v, ok := args[key]
	if !ok {
		return "", fmt.Errorf("%w: missing %q", sandbox.ErrBadRequest, key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%w: %q must be a string", sandbox.ErrBadRequest, key)
	}
	return s, nil
}

// renderUnifiedDiff builds a minimal one-hunk unified diff for a single
// contiguous edit. idx/searchLen mark the replaced region in `before`;
// replaceLen is the length of the replacement (used to locate the same
// region in `after`). 3 lines of context are emitted above and below the
// changed lines, matching the GNU diff default.
func renderUnifiedDiff(path, before, after string, idx, searchLen, replaceLen int) string {
	const ctx = 3

	beforeLines := strings.Split(before, "\n")
	afterLines := strings.Split(after, "\n")

	// Locate the changed line range in `before`. firstLine is 0-based.
	firstLine := strings.Count(before[:idx], "\n")
	lastLine := strings.Count(before[:idx+searchLen], "\n")

	// Same range in `after` — same start line, end shifted by (replaceLen-searchLen)
	// in characters which we recompute via newline count.
	afterFirst := firstLine
	afterLast := strings.Count(after[:idx+replaceLen], "\n")

	startCtx := firstLine - ctx
	if startCtx < 0 {
		startCtx = 0
	}
	endCtxBefore := lastLine + ctx
	if endCtxBefore >= len(beforeLines) {
		endCtxBefore = len(beforeLines) - 1
	}
	endCtxAfter := afterLast + ctx
	if endCtxAfter >= len(afterLines) {
		endCtxAfter = len(afterLines) - 1
	}

	// Unified-diff line counts are 1-based; "0,0" is used when a side is empty.
	beforeStart := startCtx + 1
	beforeCount := endCtxBefore - startCtx + 1
	afterStart := startCtx + 1
	afterCount := endCtxAfter - startCtx + 1

	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n", path)
	fmt.Fprintf(&b, "+++ b/%s\n", path)
	fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", beforeStart, beforeCount, afterStart, afterCount)

	// Leading context.
	for i := startCtx; i < firstLine; i++ {
		fmt.Fprintf(&b, " %s\n", beforeLines[i])
	}
	// Removed lines.
	for i := firstLine; i <= lastLine; i++ {
		fmt.Fprintf(&b, "-%s\n", beforeLines[i])
	}
	// Added lines.
	for i := afterFirst; i <= afterLast; i++ {
		fmt.Fprintf(&b, "+%s\n", afterLines[i])
	}
	// Trailing context (from the after side so line numbers are accurate
	// when the replacement changed line count).
	for i := afterLast + 1; i <= endCtxAfter; i++ {
		fmt.Fprintf(&b, " %s\n", afterLines[i])
	}
	return b.String()
}
