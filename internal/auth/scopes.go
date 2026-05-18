package auth

import "strings"

// Scope strings the orchestrator recognizes. Tokens declare their
// scope set via the `scopes` JWT claim; the dispatcher checks against
// these constants before invoking a tool.
//
// Phase 12 model: a token whose scopes do NOT mention any `tools:`
// prefix is treated as legacy-permissive — every tool is allowed.
// Once the operator adds a single `tools:*` or `tools:<name>` scope,
// strict mode kicks in and only listed tools are permitted. This
// keeps the pre-12 minting habit (`scopes=[orchestrator:connect]`)
// working without forcing a wholesale re-issue of every live token.
const (
	ScopeConnect = "orchestrator:connect" // legacy, gates /ws only

	// Wildcard. A token with `tools:*` is permitted to call any
	// tool — the strict-mode equivalent of legacy-permissive.
	ScopeToolsAll = "tools:*"

	// Per-tool scopes. Match the tool name as the dispatcher sees
	// it (sandbox.ToolExecuteScript, fsops names, github_* prefix).
	ScopeToolsExecuteScript = "tools:execute_script"
	ScopeToolsReadFile      = "tools:read_file"
	ScopeToolsListDir       = "tools:list_dir"
	ScopeToolsWritePatch    = "tools:write_patch"

	// ScopeToolsGitHub covers the entire github_* tool family.
	// Operators can also issue narrower per-tool scopes by name
	// (e.g. tools:github_create_pull_request) — HasToolScope
	// honors both shapes.
	ScopeToolsGitHub = "tools:github"

	// scopePrefixTools is the per-tool scope namespace marker.
	// Internal; callers use HasToolScope.
	scopePrefixTools = "tools:"
)

// HasToolScope reports whether a JWT scope set authorizes the given
// tool name. The two-tier policy is:
//
//  1. **Legacy-permissive.** If no scope in the set starts with
//     `tools:`, the caller is allowed every tool. Tokens minted
//     before Phase 12 (typically `scopes=[orchestrator:connect]`)
//     hit this branch — they keep working.
//
//  2. **Strict.** Once any `tools:` scope is present, only listed
//     tools (plus `tools:*` wildcard, plus the `tools:github`
//     family rule) are permitted.
//
// The github_* family rule: any tool whose name starts with
// `github_` is allowed when the scope set contains `tools:github`
// OR a more specific `tools:github_<exact_tool>` match. Per-tool
// GitHub scopes win over the family scope — a deny-by-omission on
// the more specific scope is honored.
func HasToolScope(scopes []string, tool string) bool {
	if tool == "" {
		return false
	}
	hasAnyToolsScope := false
	for _, s := range scopes {
		if strings.HasPrefix(s, scopePrefixTools) {
			hasAnyToolsScope = true
			break
		}
	}
	if !hasAnyToolsScope {
		return true // legacy-permissive
	}
	want := scopePrefixTools + tool
	for _, s := range scopes {
		if s == ScopeToolsAll || s == want {
			return true
		}
		// Family rule: tools:github authorizes every github_* tool.
		if s == ScopeToolsGitHub && strings.HasPrefix(tool, "github_") {
			return true
		}
	}
	return false
}
