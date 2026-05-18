package githubmcp

import "strings"

// destructiveVerbs is the prefix set the heuristic treats as state-mutating.
// Sourced from the upstream tool catalogue: every tool whose name begins with
// one of these verbs writes to GitHub (creates, mutates, deletes, runs, or
// resolves). Read-only tools (get_*, list_*, search_*, *_read) fall through.
//
// Two write-only suffixes (`_write`) and the special `push_files` /
// `fork_repository` / `merge_pull_request` / `request_copilot_review` /
// `enable_pr_auto_merge` / `disable_pr_auto_merge` /
// `update_pull_request_branch` / `run_secret_scanning` cases are
// handled explicitly below.
var destructiveVerbs = []string{
	"create_",
	"update_",
	"delete_",
	"merge_",
	"push_",
	"add_",
	"enable_",
	"disable_",
	"fork_",
	"resolve_",
	"unresolve_",
	"request_",
	"run_",
	"cancel_",
	"approve_",
	"dismiss_",
	"set_",
	"remove_",
}

// destructiveExact is the explicit allowlist for tools whose names don't start
// with one of the destructive verbs but still mutate state. Kept in sync with
// the upstream catalogue.
var destructiveExact = map[string]struct{}{
	"sub_issue_write":           {},
	"issue_write":               {},
	"pull_request_review_write": {},
}

// IsDestructiveTool reports whether the named tool mutates remote state.
// Accepts either the bare upstream name ("create_issue") or the prefixed
// middleware name ("github_create_issue"). Used by the factory to
// auto-populate the approval-required allowlist.
//
// The heuristic is intentionally inclusive: false positives waste an
// approval prompt; false negatives skip the gate. Anything ambiguous is
// treated as destructive.
func IsDestructiveTool(name string) bool {
	bare := strings.TrimPrefix(name, ToolPrefix)
	if _, ok := destructiveExact[bare]; ok {
		return true
	}
	for _, v := range destructiveVerbs {
		if strings.HasPrefix(bare, v) {
			return true
		}
	}
	// *_write suffix catches future "<noun>_write" tools the upstream adds.
	if strings.HasSuffix(bare, "_write") {
		return true
	}
	return false
}
