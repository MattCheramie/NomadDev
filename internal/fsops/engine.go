// Package fsops implements the filesystem-only tool dispatch path for
// read_file, list_dir, and write_patch. These tools run as native Go on the
// orchestrator's workspace directory rather than through a sandbox container
// because they're trivial filesystem ops and the trust boundary is identical
// (the orchestrator user owns the workspace either way).
//
// fsops emits sandbox.ExecChunk frames so the orchestrator's wsserver layer
// can pipe results through the same emitChunk/emitResult machinery that
// handles container output. One wire shape across all tools.
package fsops

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

// Engine is the fsops dispatcher rooted at a host workspace directory.
// All path arguments to Run/Dispatch helpers are resolved relative to root
// with strict containment: no .. escapes, no absolute paths, no symlinks
// pointing outside the root.
type Engine struct {
	root string
}

// New constructs an Engine. root is absolute-cleaned at construction; later
// callers don't need to worry about relative or normalized paths in the
// Engine itself.
func New(root string) (*Engine, error) {
	if root == "" {
		return nil, errors.New("fsops: empty root")
	}
	abs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return nil, fmt.Errorf("fsops: abs %q: %w", root, err)
	}
	return &Engine{root: abs}, nil
}

// Root returns the absolute workspace directory.
func (e *Engine) Root() string { return e.root }

// ErrPathEscape is returned when a requested path resolves outside Engine.root.
var ErrPathEscape = errors.New("fsops: path escapes workspace root")

// resolveSafe joins rel onto e.root and verifies the result, after symlink
// resolution, stays inside the root. For non-existent paths (the write_patch
// case) it falls back to validating the parent directory — the file itself
// will be created by the caller after this returns.
//
// Returns the absolute resolved path on success.
func (e *Engine) resolveSafe(rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("%w: empty path", ErrPathEscape)
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: absolute path %q", ErrPathEscape, rel)
	}
	// Reject any ".." component up-front. filepath.Clean would silently
	// normalize a leading "../foo" to "foo" once we anchor at "/", masking
	// what was clearly an escape attempt. Tighter to refuse outright.
	cleaned := filepath.Clean(rel)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") ||
		strings.Contains(cleaned, "/../") || strings.HasSuffix(cleaned, "/..") {
		return "", fmt.Errorf("%w: %q contains '..'", ErrPathEscape, rel)
	}
	candidate := filepath.Join(e.root, cleaned)

	real, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		// Path doesn't exist yet — validate the parent directory.
		parent := filepath.Dir(candidate)
		realParent, perr := filepath.EvalSymlinks(parent)
		if perr != nil {
			return "", fmt.Errorf("%w: parent %q: %v", ErrPathEscape, parent, perr)
		}
		if !withinRoot(e.root, realParent) {
			return "", fmt.Errorf("%w: parent %q resolves outside root", ErrPathEscape, parent)
		}
		// The file's not there yet but the parent is safely inside root —
		// return the original (un-realpathed) candidate so the caller writes
		// to the requested path, not into a symlink target's parent.
		return candidate, nil
	}
	if !withinRoot(e.root, real) {
		return "", fmt.Errorf("%w: %q resolves to %q", ErrPathEscape, rel, real)
	}
	return real, nil
}

// withinRoot returns true when target is root itself or a descendant of root.
func withinRoot(root, target string) bool {
	if target == root {
		return true
	}
	sep := string(filepath.Separator)
	return strings.HasPrefix(target+sep, root+sep)
}
