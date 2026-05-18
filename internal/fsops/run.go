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
	ToolReadFile   = "read_file"
	ToolListDir    = "list_dir"
	ToolWritePatch = "write_patch"
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
