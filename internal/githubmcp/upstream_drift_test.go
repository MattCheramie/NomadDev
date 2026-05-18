//go:build github

package githubmcp

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// TestUpstreamDrift_ListAndParseSchemas runs against a real, **latest**
// github-mcp-server subprocess and asserts the adapter's invariants:
//
//  1. tools/list returns a non-empty catalogue.
//  2. Every Tool.InputSchema round-trips through our ConvertSchema without
//     a fatal error (free-form fallback is OK; silent loss isn't tested).
//  3. The destructive-tool set isn't empty (at least one tool is gated).
//  4. No read-only tool (get_/list_/search_/*_read) is reported destructive.
//
// Designed for the upstream-drift CI workflow: a placeholder token is
// sufficient because tools/list doesn't hit the real API. The test only
// runs when NOMADDEV_GITHUB_TOKEN is set (so it skips in the default
// test-github local run, mirroring TestLive_*).
//
// When this fails, the diagnostic in the log tells us exactly which
// invariant broke — bump the pinned GITHUB_MCP_VERSION (or update the
// adapter) before merging the next release.
func TestUpstreamDrift_ListAndParseSchemas(t *testing.T) {
	if os.Getenv("NOMADDEV_GITHUB_TOKEN") == "" {
		t.Skip("NOMADDEV_GITHUB_TOKEN not set; skipping upstream drift smoke")
	}
	if _, err := resolveBinary(os.Getenv("NOMADDEV_GITHUB_MCP_BIN")); err != nil {
		t.Skipf("github-mcp-server binary not available: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	caller, err := New(ctx, Options{
		Token: EnvTokenSource{Var: "NOMADDEV_GITHUB_TOKEN"},
		// Run the full surface so any added/renamed tool in any toolset
		// trips the drift check.
		Toolsets:     []string{"all"},
		StartTimeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = caller.Close() })

	tools, err := caller.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	// Invariant 1: non-empty catalogue.
	if len(tools) == 0 {
		t.Fatal("DRIFT: tools/list returned 0 tools; upstream may have changed handshake")
	}
	t.Logf("upstream advertises %d tools", len(tools))

	// Invariant 2: every schema is convertible. ConvertSchema is tolerant
	// — it returns a free-form Schema on parse failure — but the conversion
	// itself must complete without panic. We also re-marshal each ToolSpec
	// to confirm it survives the Gemini-bound JSON path.
	for _, ts := range tools {
		raw, err := json.Marshal(ts.Parameters)
		if err != nil {
			t.Errorf("DRIFT: tool %q schema does not marshal: %v", ts.Name, err)
		}
		if len(raw) == 0 {
			t.Errorf("DRIFT: tool %q schema marshalled to zero bytes", ts.Name)
		}
		if ts.Name == "" || !strings.HasPrefix(ts.Name, ToolPrefix) {
			t.Errorf("DRIFT: tool name %q missing %q prefix", ts.Name, ToolPrefix)
		}
	}

	// Invariant 3: at least one tool gated as destructive. If this hits
	// zero either the upstream stripped its annotations or the verb
	// heuristic is silently failing — either is a real drift.
	destructiveCount := 0
	for _, ts := range tools {
		if caller.IsDestructive(ts.Name) {
			destructiveCount++
		}
	}
	if destructiveCount == 0 {
		t.Error("DRIFT: no destructive tools detected; approval gate would auto-grant every write")
	}
	t.Logf("destructive tools: %d / %d", destructiveCount, len(tools))

	// Invariant 4: read-only-looking tools must NOT be destructive. If
	// the heuristic over-counts, the approval gate would block reads
	// (annoying UX) — caught here.
	for _, ts := range tools {
		bare := UnprefixedName(ts.Name)
		obviouslyReadOnly := strings.HasPrefix(bare, "get_") ||
			strings.HasPrefix(bare, "list_") ||
			strings.HasPrefix(bare, "search_") ||
			strings.HasSuffix(bare, "_read")
		if obviouslyReadOnly && caller.IsDestructive(ts.Name) {
			t.Errorf("DRIFT: tool %q is obviously read-only but reported destructive", ts.Name)
		}
	}
}
