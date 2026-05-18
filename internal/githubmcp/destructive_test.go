package githubmcp

import "testing"

func TestIsDestructiveTool(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		// Mutating verbs (prefixed and bare both accepted).
		{"create_issue", true},
		{"github_create_pull_request", true},
		{"update_pull_request_branch", true},
		{"delete_file", true},
		{"merge_pull_request", true},
		{"push_files", true},
		{"add_issue_comment", true},
		{"enable_pr_auto_merge", true},
		{"disable_pr_auto_merge", true},
		{"fork_repository", true},
		{"resolve_review_thread", true},
		{"unresolve_review_thread", true},
		{"request_copilot_review", true},
		{"run_secret_scanning", true},
		{"create_branch", true},
		{"create_or_update_file", true},
		{"create_repository", true},

		// Explicit *_write suffix tools.
		{"issue_write", true},
		{"sub_issue_write", true},
		{"pull_request_review_write", true},

		// Future-proofing: any new *_write tool should be auto-gated.
		{"some_new_thing_write", true},

		// Read-only — must not be gated.
		{"get_me", false},
		{"github_get_me", false},
		{"get_file_contents", false},
		{"list_branches", false},
		{"list_pull_requests", false},
		{"search_repositories", false},
		{"search_code", false},
		{"issue_read", false},
		{"pull_request_read", false},
		{"get_commit", false},
	}
	for _, tc := range cases {
		if got := IsDestructiveTool(tc.name); got != tc.want {
			t.Errorf("IsDestructiveTool(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
