//go:build github

package githubmcp

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mattcheramie/nomaddev/internal/middleware"
)

// requireLive skips when NOMADDEV_GITHUB_TOKEN is absent or when the upstream
// github-mcp-server binary is not on PATH (and no override is set). CI never
// sets the token; developers opt in by exporting it locally.
//
// Mirrors the gemini_test.go requireKey pattern: same build tag, same opt-in
// shape, no separate `github_live` tag to manage.
func requireLive(t *testing.T) string {
	t.Helper()
	tok := os.Getenv("NOMADDEV_GITHUB_TOKEN")
	if tok == "" {
		t.Skip("NOMADDEV_GITHUB_TOKEN not set; skipping live MCP round-trip")
	}
	if _, err := resolveBinary(os.Getenv("NOMADDEV_GITHUB_MCP_BIN")); err != nil {
		t.Skipf("github-mcp-server binary not available: %v", err)
	}
	return tok
}

// TestLive_HandshakeAndListTools exercises the full subprocess lifecycle:
// spawn binary → initialize handshake → tools/list → Close. Sanity-checks
// that the upstream API hasn't broken in a way our adapter would miss.
//
// Skipped in CI; manual smoke for developers.
func TestLive_HandshakeAndListTools(t *testing.T) {
	_ = requireLive(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	caller, err := New(ctx, Options{
		Token:        EnvTokenSource{Var: "NOMADDEV_GITHUB_TOKEN"},
		Toolsets:     []string{"context", "users"}, // narrow surface for a fast smoke
		StartTimeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = caller.Close() })

	tools, err := caller.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) == 0 {
		t.Fatal("ListTools returned empty catalogue")
	}

	// Every name must carry the github_ prefix and have a non-empty
	// description — these are the two fields Gemini's function-calling layer
	// reads from each ToolSpec.
	for _, ts := range tools {
		if !strings.HasPrefix(ts.Name, ToolPrefix) {
			t.Errorf("tool %q missing %q prefix", ts.Name, ToolPrefix)
		}
		if ts.Description == "" {
			t.Errorf("tool %q has empty description", ts.Name)
		}
	}

	// IsDestructive must agree with the destructiveVerbs heuristic for at
	// least one well-known mutator if it appears in the narrowed surface.
	// We don't require it (some toolsets are entirely read-only) — just
	// assert internal consistency.
	for _, ts := range tools {
		want := IsDestructiveTool(ts.Name)
		got := caller.IsDestructive(ts.Name)
		// The upstream's DestructiveHint annotation can override either way;
		// we only require that read_/list_/get_/search_ tools never come
		// back as destructive (those would be a bug in our heuristic or a
		// surprising upstream annotation).
		bare := UnprefixedName(ts.Name)
		isObviouslyReadOnly := strings.HasPrefix(bare, "get_") ||
			strings.HasPrefix(bare, "list_") ||
			strings.HasPrefix(bare, "search_") ||
			strings.HasSuffix(bare, "_read")
		if isObviouslyReadOnly && got {
			t.Errorf("tool %q reported destructive but is obviously read-only", ts.Name)
		}
		_ = want
	}
}

// TestLive_GetMe is the smallest end-to-end round-trip: spawns the server,
// invokes the get_me tool, expects a non-error CallToolResult with a text
// content block. Confirms the Call() → CallToolParams → CallToolResult →
// ExecChunk encoding path works against a real subprocess.
func TestLive_GetMe(t *testing.T) {
	_ = requireLive(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	caller, err := New(ctx, Options{
		Token:        EnvTokenSource{Var: "NOMADDEV_GITHUB_TOKEN"},
		Toolsets:     []string{"context", "users"},
		StartTimeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = caller.Close() })

	ch, err := caller.Call(ctx, middleware.ToolCall{
		ID:   "live-1",
		Tool: PrefixedName("get_me"),
	}, middleware.DispatchOptions{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	var stdout []byte
	var exitCode int
	var exitErr error
	for chunk := range ch {
		switch chunk.Stream {
		case "stdout":
			stdout = append(stdout, chunk.Data...)
		case "exit":
			exitCode = chunk.ExitCode
			exitErr = chunk.Err
		}
	}
	if exitErr != nil {
		t.Fatalf("exit chunk err: %v", exitErr)
	}
	if exitCode != 0 {
		t.Fatalf("get_me returned exit %d, body=%s", exitCode, string(stdout))
	}
	if len(stdout) == 0 {
		t.Fatal("get_me produced empty payload")
	}
	// The encoded payload is JSON {content:[{type,text}], structured?, is_error?}.
	// We don't parse — just confirm it looks like JSON the wsserver can hand
	// to the translator unmodified.
	if stdout[0] != '{' {
		t.Errorf("payload doesn't start with '{': %s", string(stdout))
	}
}
