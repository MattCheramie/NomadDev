package sandbox

import (
	"strings"
	"testing"
)

// Phase 10.2: sanitizeSID maps a JWT sid claim to a path-safe
// subdirectory name. Defense in depth — the wsserver layer already
// constrains sids to JWT-issued shapes, but a malformed or hostile
// claim must not escape the workspace root.

func TestSanitizeSID_AllowsCommonChars(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"sess-1", "sess-1"},
		{"abc_123", "abc_123"},
		{"matt.dev", "matt.dev"},
		{"sess-2026-05-18T12-34-56Z", "sess-2026-05-18T12-34-56Z"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := sanitizeSID(tc.in); got != tc.want {
			t.Errorf("sanitizeSID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSanitizeSID_StripsPathTraversal(t *testing.T) {
	// `..` is the textbook traversal token. Collapsing it to `__`
	// keeps the SID identifiable in error logs while neutralizing
	// the directory-walking shape.
	cases := []struct {
		in, want string
	}{
		{"..", "__"},
		{"../etc", "__/etc-but-no-slash"}, // see specific case below
		{"foo..bar", "foo__bar"},
		{"../../passwd", "____/passwd-shape-stripped"},
	}
	// Each test individually because the want string above is
	// illustrative; the real assertions are below.
	_ = cases

	if got := sanitizeSID(".."); strings.Contains(got, "..") {
		t.Errorf("`..` survived sanitization as %q", got)
	}
	if got := sanitizeSID("foo..bar"); strings.Contains(got, "..") {
		t.Errorf("`foo..bar` survived sanitization as %q", got)
	}
	if got := sanitizeSID("../etc/passwd"); strings.Contains(got, "..") {
		t.Errorf("`../etc/passwd` survived sanitization as %q", got)
	}
	// Slashes get mapped to underscore so the joined path has
	// no extra separators that filepath.Join would respect.
	if got := sanitizeSID("../etc"); strings.Contains(got, "/") {
		t.Errorf("`../etc` produced a slash in %q", got)
	}
}

func TestSanitizeSID_StripsNonAllowedChars(t *testing.T) {
	if got := sanitizeSID("a b\tc"); strings.ContainsAny(got, " \t") {
		t.Errorf("whitespace survived in %q", got)
	}
	if got := sanitizeSID("a/b\\c"); strings.ContainsAny(got, "/\\") {
		t.Errorf("path separators survived in %q", got)
	}
	if got := sanitizeSID("a$b@c%d"); strings.ContainsAny(got, "$@%") {
		t.Errorf("shell-meta survived in %q", got)
	}
}

func TestSanitizeSID_CapsLength(t *testing.T) {
	huge := strings.Repeat("x", 1024)
	got := sanitizeSID(huge)
	if len(got) != 64 {
		t.Errorf("len = %d, want 64 (cap)", len(got))
	}
}
