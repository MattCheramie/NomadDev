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
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Engine is the fsops dispatcher rooted at a host workspace directory.
// All path arguments to Run/Dispatch helpers are resolved relative to root
// with strict containment: no .. escapes, no absolute paths, no symlinks
// pointing outside the root.
//
// When PerSession is set, path resolution additionally prepends a
// per-SID subdirectory (`<root>/<sanitized-sid>/`) so concurrent
// sessions can't see each other's files via fsops. The SID is
// supplied per call via WithSessionID(ctx, sid). Empty / absent SID
// falls back to the shared root — back-compat for non-multi-tenant
// deploys.
type Engine struct {
	root       string
	perSession bool
}

// New constructs an Engine. root is absolute-cleaned at construction; later
// callers don't need to worry about relative or normalized paths in the
// Engine itself.
func New(root string) (*Engine, error) {
	return NewWithOptions(root, false)
}

// NewWithOptions is New plus the per-session-isolation toggle. When
// perSession is true, every call's resolved path is scoped to a
// per-SID subdir of root (Phase 12.2). Matches the sandbox runner's
// PerSessionWorkspace knob so both backends isolate in lockstep.
func NewWithOptions(root string, perSession bool) (*Engine, error) {
	if root == "" {
		return nil, errors.New("fsops: empty root")
	}
	abs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return nil, fmt.Errorf("fsops: abs %q: %w", root, err)
	}
	return &Engine{root: abs, perSession: perSession}, nil
}

// Root returns the absolute workspace directory.
func (e *Engine) Root() string { return e.root }

// PerSession reports whether the engine scopes paths per SID.
func (e *Engine) PerSession() bool { return e.perSession }

// ctxSIDKey is the context-key type for per-session SID propagation.
type ctxSIDKey struct{}

// WithSessionID attaches a session id to ctx. Engines created with
// PerSession=true use it to scope path resolution to
// `<root>/<sanitized-sid>/`. Empty sid is a no-op.
func WithSessionID(ctx context.Context, sid string) context.Context {
	if sid == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxSIDKey{}, sid)
}

func sessionFromCtx(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	s, _ := ctx.Value(ctxSIDKey{}).(string)
	return s
}

// rootForCtx returns the effective root for the given context. In
// per-session mode it joins root with the sanitized SID and
// MkdirAll's at 0o700 on first use; otherwise it returns e.root
// unchanged. Returns an error if the per-session mkdir fails — the
// caller surfaces it to the wire layer as a fsops error.
func (e *Engine) rootForCtx(ctx context.Context) (string, error) {
	if !e.perSession {
		return e.root, nil
	}
	sid := sanitizeSID(sessionFromCtx(ctx))
	if sid == "" {
		// Back-compat: a caller that didn't set the SID falls
		// through to the shared root rather than failing the call.
		// Strict-mode deployments wire SID at every entry point;
		// callers without one are tests and the legacy direct path.
		return e.root, nil
	}
	scoped := filepath.Join(e.root, sid)
	if err := os.MkdirAll(scoped, 0o700); err != nil {
		return "", fmt.Errorf("fsops: per-session mkdir %q: %w", scoped, err)
	}
	return scoped, nil
}

// sanitizeSID maps a JWT sid claim to a path-safe subdirectory name.
// Mirrors sandbox.sanitizeSID — duplicated rather than shared via a
// new package because the algorithm is 30 lines and both packages
// are leaves. If a third caller appears, refactor to a shared
// helper.
func sanitizeSID(sid string) string {
	if sid == "" {
		return ""
	}
	const maxLen = 64
	b := make([]byte, 0, len(sid))
	for i := 0; i < len(sid) && len(b) < maxLen; i++ {
		c := sid[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '-' || c == '_' || c == '.':
			if c == '.' && len(b) > 0 && b[len(b)-1] == '.' {
				b[len(b)-1] = '_'
				b = append(b, '_')
				continue
			}
			b = append(b, c)
		default:
			b = append(b, '_')
		}
	}
	return string(b)
}

// ErrPathEscape is returned when a requested path resolves outside Engine.root.
var ErrPathEscape = errors.New("fsops: path escapes workspace root")

// resolveSafe joins rel onto the effective root for ctx (which is
// Engine.root for non-per-session or `<root>/<sid>` in per-session
// mode) and verifies the result, after symlink resolution, stays
// inside that scope. For non-existent paths (the write_patch case)
// it falls back to validating the parent directory — the file
// itself will be created by the caller after this returns.
//
// Returns the absolute resolved path on success.
func (e *Engine) resolveSafe(ctx context.Context, rel string) (string, error) {
	scopedRoot, err := e.rootForCtx(ctx)
	if err != nil {
		return "", err
	}
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
	candidate := filepath.Join(scopedRoot, cleaned)

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
		if !withinRoot(scopedRoot, realParent) {
			return "", fmt.Errorf("%w: parent %q resolves outside root", ErrPathEscape, parent)
		}
		// The file's not there yet but the parent is safely inside root —
		// return the original (un-realpathed) candidate so the caller writes
		// to the requested path, not into a symlink target's parent.
		return candidate, nil
	}
	if !withinRoot(scopedRoot, real) {
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
