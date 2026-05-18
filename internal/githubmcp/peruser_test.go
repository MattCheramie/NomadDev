package githubmcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeUserTokens(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write tokens file: %v", err)
	}
	return path
}

func TestPerUserTokenSource_LookupByCtxSub(t *testing.T) {
	path := writeUserTokens(t, `{"alice":"ghp_a","bob":"ghp_b"}`)
	src := &PerUserTokenSource{Path: path, Fallback: StaticTokenSource{Value: "ghp_default"}}

	cases := []struct {
		sub, want string
	}{
		{"alice", "ghp_a"},
		{"bob", "ghp_b"},
		{"carol", "ghp_default"}, // miss → fallback
		{"", "ghp_default"},      // no sub on ctx → fallback
	}
	for _, tc := range cases {
		ctx := WithUserSub(context.Background(), tc.sub)
		got, err := src.Token(ctx)
		if err != nil {
			t.Errorf("sub=%q: err = %v", tc.sub, err)
			continue
		}
		if got != tc.want {
			t.Errorf("sub=%q: token = %q, want %q", tc.sub, got, tc.want)
		}
	}
}

func TestPerUserTokenSource_NoFallback_MissReturnsErrNoToken(t *testing.T) {
	path := writeUserTokens(t, `{"alice":"x"}`)
	src := &PerUserTokenSource{Path: path}
	ctx := WithUserSub(context.Background(), "carol")
	_, err := src.Token(ctx)
	if !errors.Is(err, ErrNoToken) {
		t.Fatalf("want ErrNoToken on fallback-less miss, got %v", err)
	}
}

func TestPerUserTokenSource_FileMissing_FallsThroughToFallback(t *testing.T) {
	src := &PerUserTokenSource{
		Path:     "/definitely/does/not/exist.json",
		Fallback: StaticTokenSource{Value: "ghp_fallback"},
	}
	ctx := WithUserSub(context.Background(), "alice")
	got, err := src.Token(ctx)
	if err != nil || got != "ghp_fallback" {
		t.Fatalf("got=%q err=%v, want fallback hit", got, err)
	}
}

func TestPerUserTokenSource_BadJSON_FallsThroughToFallback(t *testing.T) {
	path := writeUserTokens(t, `{not json`)
	src := &PerUserTokenSource{Path: path, Fallback: StaticTokenSource{Value: "ghp_fallback"}}
	got, err := src.Token(WithUserSub(context.Background(), "alice"))
	if err != nil || got != "ghp_fallback" {
		t.Fatalf("got=%q err=%v, want fallback on parse error", got, err)
	}
}

func TestPerUserTokenSource_HotReload(t *testing.T) {
	path := writeUserTokens(t, `{"alice":"v1"}`)
	src := &PerUserTokenSource{Path: path}

	// First read.
	tok, err := src.Token(WithUserSub(context.Background(), "alice"))
	if err != nil || tok != "v1" {
		t.Fatalf("v1: got=%q err=%v", tok, err)
	}

	// Rewrite with a new value + bump mtime to ensure the cache invalidates.
	time.Sleep(20 * time.Millisecond) // ensure mtime tick
	if err := os.WriteFile(path, []byte(`{"alice":"v2"}`), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	// Force a new mtime even on filesystems with coarse resolution.
	newMtime := time.Now().Add(time.Second)
	if err := os.Chtimes(path, newMtime, newMtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	tok, err = src.Token(WithUserSub(context.Background(), "alice"))
	if err != nil || tok != "v2" {
		t.Fatalf("v2 after rewrite: got=%q err=%v", tok, err)
	}
}

func TestPerUserTokenSource_EmptyValueDoesNotMatch(t *testing.T) {
	// An empty per-user entry is treated as "no token for this user" so the
	// fallback is consulted. Catches accidental {"alice":""} entries.
	path := writeUserTokens(t, `{"alice":""}`)
	src := &PerUserTokenSource{Path: path, Fallback: StaticTokenSource{Value: "ghp_fallback"}}
	got, err := src.Token(WithUserSub(context.Background(), "alice"))
	if err != nil || got != "ghp_fallback" {
		t.Fatalf("got=%q err=%v, want fallback on empty entry", got, err)
	}
}
