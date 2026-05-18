package fsops

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Phase 12.2: per-session isolation. When NewWithOptions(_, true)
// is configured, a write_patch with WithSessionID(ctx, "alice")
// lands in <root>/alice/, separate from "bob".

func TestPerSession_PathScopedToSID(t *testing.T) {
	root := t.TempDir()
	e, err := NewWithOptions(root, true)
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}

	// Resolve the same "foo" path under two different SIDs.
	ctxA := WithSessionID(context.Background(), "alice")
	pathA, err := e.resolveSafe(ctxA, "foo")
	if err != nil {
		t.Fatalf("resolveSafe alice: %v", err)
	}
	ctxB := WithSessionID(context.Background(), "bob")
	pathB, err := e.resolveSafe(ctxB, "foo")
	if err != nil {
		t.Fatalf("resolveSafe bob: %v", err)
	}

	if pathA == pathB {
		t.Errorf("paths for different SIDs collided: both = %q", pathA)
	}
	if filepath.Base(filepath.Dir(pathA)) != "alice" {
		t.Errorf("alice path = %q, want under .../alice/", pathA)
	}
	if filepath.Base(filepath.Dir(pathB)) != "bob" {
		t.Errorf("bob path = %q, want under .../bob/", pathB)
	}

	// Per-SID subdir must exist with 0o700.
	st, err := os.Stat(filepath.Dir(pathA))
	if err != nil {
		t.Fatalf("stat alice dir: %v", err)
	}
	if st.Mode().Perm() != 0o700 {
		t.Errorf("alice dir mode = %o, want 0o700", st.Mode().Perm())
	}
}

func TestPerSession_EmptySIDFallsBackToSharedRoot(t *testing.T) {
	// Back-compat: a per-session engine that gets an empty SID
	// (legacy callers, cmd/sandbox direct path) routes to the
	// shared root rather than failing the call.
	root := t.TempDir()
	e, _ := NewWithOptions(root, true)
	path, err := e.resolveSafe(context.Background(), "foo")
	if err != nil {
		t.Fatalf("resolveSafe empty-sid: %v", err)
	}
	if filepath.Dir(path) != filepath.Clean(root) {
		t.Errorf("empty-sid path = %q, want under shared root %q",
			path, root)
	}
}

func TestPerSession_OffPreservesSharedRoot(t *testing.T) {
	// With perSession=false (default), SID context is ignored —
	// the engine continues to root every call at e.root.
	root := t.TempDir()
	e, _ := NewWithOptions(root, false)
	ctx := WithSessionID(context.Background(), "alice")
	path, err := e.resolveSafe(ctx, "foo")
	if err != nil {
		t.Fatalf("resolveSafe: %v", err)
	}
	if filepath.Dir(path) != filepath.Clean(root) {
		t.Errorf("perSession=false path = %q, want under shared root %q",
			path, root)
	}
}

func TestPerSession_RejectsTraversalUnderSID(t *testing.T) {
	// Defense in depth: the existing ".." rejection still applies
	// once a SID is in play. A path like "../etc" must not escape
	// the per-SID subdir.
	root := t.TempDir()
	e, _ := NewWithOptions(root, true)
	ctx := WithSessionID(context.Background(), "alice")
	if _, err := e.resolveSafe(ctx, "../etc/passwd"); err == nil {
		t.Fatal("expected resolveSafe to reject ..-traversal under per-session root")
	}
}

func TestSanitizeSID_FsopsCollapsesDotDot(t *testing.T) {
	// Internal sanity check matching the sandbox sanitizer's contract:
	// `..` must not survive sanitization.
	cases := []string{"..", "../../etc", "foo..bar"}
	for _, in := range cases {
		got := sanitizeSID(in)
		for i := 0; i+1 < len(got); i++ {
			if got[i] == '.' && got[i+1] == '.' {
				t.Errorf("sanitizeSID(%q) = %q still contains '..'", in, got)
			}
		}
	}
}
