package auth

import "testing"

func TestHasToolScope_LegacyPermissive(t *testing.T) {
	// Token with no tools:* scopes at all (typical pre-Phase-12 mint)
	// allows every tool.
	scopes := []string{ScopeConnect}
	for _, tool := range []string{"execute_script", "read_file", "github_create_pull_request"} {
		if !HasToolScope(scopes, tool) {
			t.Errorf("legacy token rejected for %q", tool)
		}
	}
}

func TestHasToolScope_EmptyScopesLegacyPermissive(t *testing.T) {
	// Empty + nil are both back-compat — they pre-date Phase 12.
	for _, s := range [][]string{nil, {}, {""}} {
		if !HasToolScope(s, "execute_script") {
			t.Errorf("empty scopes (%v) rejected", s)
		}
	}
}

func TestHasToolScope_StrictMode_WildcardAllowsAll(t *testing.T) {
	scopes := []string{ScopeConnect, ScopeToolsAll}
	for _, tool := range []string{"execute_script", "read_file", "github_create_pull_request"} {
		if !HasToolScope(scopes, tool) {
			t.Errorf("wildcard scope rejected %q", tool)
		}
	}
}

func TestHasToolScope_StrictMode_PerTool(t *testing.T) {
	scopes := []string{ScopeConnect, ScopeToolsExecuteScript}
	if !HasToolScope(scopes, "execute_script") {
		t.Error("allowed tool rejected")
	}
	if HasToolScope(scopes, "read_file") {
		t.Error("unlisted tool allowed")
	}
	if HasToolScope(scopes, "github_create_pull_request") {
		t.Error("github tool allowed without tools:github or wildcard")
	}
}

func TestHasToolScope_GitHubFamilyRule(t *testing.T) {
	scopes := []string{ScopeConnect, ScopeToolsGitHub}
	// tools:github authorizes the whole github_* family.
	for _, tool := range []string{"github_get_me", "github_create_pull_request", "github_list_issues"} {
		if !HasToolScope(scopes, tool) {
			t.Errorf("github family scope rejected %q", tool)
		}
	}
	// But not non-github tools.
	if HasToolScope(scopes, "execute_script") {
		t.Error("github family scope allowed a non-github tool")
	}
}

func TestHasToolScope_PerToolGitHubOverridesFamily(t *testing.T) {
	// Operator wants ONLY github_get_me, not the rest of the family.
	// They mint `tools:github_get_me` and omit `tools:github`. The
	// dispatcher must respect the omission.
	scopes := []string{ScopeConnect, "tools:github_get_me"}
	if !HasToolScope(scopes, "github_get_me") {
		t.Error("per-tool github scope rejected its own tool")
	}
	if HasToolScope(scopes, "github_create_pull_request") {
		t.Error("per-tool github scope allowed a sibling tool")
	}
}

func TestHasToolScope_EmptyToolNeverAllowed(t *testing.T) {
	// Defensive: an empty tool name means a programming error
	// upstream — never allow it through.
	if HasToolScope([]string{ScopeToolsAll}, "") {
		t.Error("empty tool name was authorized")
	}
}
