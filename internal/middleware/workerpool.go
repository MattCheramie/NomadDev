package middleware

import (
	"fmt"
	"path"
	"strings"
)

// ToolDispatchWorkerPool is the tool name for the concurrent worker-pool
// dispatcher. Calling it splits a large migration into sub-tasks that each run
// headlessly in an isolated git worktree before being merged back.
const ToolDispatchWorkerPool = "dispatch_worker_pool"

// maxSubTaskPromptBytes caps one sub-task prompt so a single
// dispatch_worker_pool call cannot blow the prompt window.
const maxSubTaskPromptBytes = 16 * 1024

// Worker task status values reported in WorkerTaskResult.Status.
const (
	WorkerStatusSuccess        = "success"
	WorkerStatusFailed         = "failed"
	WorkerStatusScopeViolation = "scope_violation"
	WorkerStatusCanceled       = "canceled"
)

// SubTask is one element of the dispatch_worker_pool `tasks` array.
type SubTask struct {
	ID     string
	Prompt string
	// Paths is the declared file/directory scope this sub-task may modify.
	// Scopes are validated pairwise-disjoint across all tasks so the
	// merge-back can never conflict.
	Paths []string
}

// WorkerPoolArgs is the decoded, validated dispatch_worker_pool argument
// object.
type WorkerPoolArgs struct {
	Tasks []SubTask
}

// WorkerTaskResult is the per-task outcome handed back in the aggregate.
type WorkerTaskResult struct {
	TaskID      string `json:"task_id"`
	Branch      string `json:"branch"`
	Status      string `json:"status"`
	Summary     string `json:"summary,omitempty"`
	MergeStatus string `json:"merge_status"`
	MergedSHA   string `json:"merged_sha,omitempty"`
	Usage       Usage  `json:"usage"`
	Error       string `json:"error,omitempty"`
}

// WorkerPoolResult is the aggregate dispatch_worker_pool result fed back to
// the parent translator and persisted to history.
type WorkerPoolResult struct {
	BaseSHA    string             `json:"base_sha"`
	PrimaryRef string             `json:"primary_ref"`
	Tasks      []WorkerTaskResult `json:"tasks"`
}

// WorkerPoolSpec returns the ToolSpec for dispatch_worker_pool. It is kept
// separate from DefaultTools() so the factory can append it conditionally
// (the feature is opt-in) and so sub-dispatcher catalogues can exclude it by
// identity.
func WorkerPoolSpec() ToolSpec {
	return ToolSpec{
		Name: ToolDispatchWorkerPool,
		Description: "Run several code-migration sub-tasks concurrently to speed up a " +
			"large migration. Each sub-task executes headlessly in its own isolated git " +
			"worktree and temporary branch, seeded with the current conversation context; " +
			"the finished branches are then merged back into the primary branch. Every " +
			"sub-task must declare in 'paths' the files or directories it will modify, and " +
			"those scopes must be disjoint — two sub-tasks may never touch the same file. " +
			"Each sub-task's edits still require human approval. Requires human approval to " +
			"launch.",
		Parameters: Schema{
			Type: "object",
			Properties: map[string]*Schema{
				"tasks": {
					Type:        "array",
					Description: "the sub-tasks to run in parallel",
					Items: &Schema{
						Type: "object",
						Properties: map[string]*Schema{
							"id": {Type: "string", Description: "optional stable id for the sub-task"},
							"prompt": {Type: "string",
								Description: "the instruction this sub-task's headless dispatcher will execute"},
							"paths": {
								Type:        "array",
								Items:       &Schema{Type: "string"},
								Description: "files/directories this sub-task may modify, relative to the workspace root; must be disjoint from every other sub-task",
							},
						},
						Required: []string{"prompt", "paths"},
					},
				},
			},
			Required: []string{"tasks"},
		},
	}
}

// SubDispatcherTools returns the catalogue with dispatch_worker_pool removed.
// This is the fork-bomb guard: a headless sub-dispatcher must not be able to
// spawn its own worker pool.
func SubDispatcherTools(all []ToolSpec) []ToolSpec {
	out := make([]ToolSpec, 0, len(all))
	for _, t := range all {
		if t.Name == ToolDispatchWorkerPool {
			continue
		}
		out = append(out, t)
	}
	return out
}

// validateDispatchWorkerPool is the tools.go Validate hook. It performs the
// full structural check (the real per-call task cap is applied later by the
// orchestrator, hence maxTasks=0 here).
func validateDispatchWorkerPool(args map[string]any) error {
	_, err := ParseWorkerPoolArgs(args, 0)
	return err
}

// ParseWorkerPoolArgs decodes and validates dispatch_worker_pool arguments.
// maxTasks caps the task-array length (0 = no cap). It enforces non-empty
// prompts and scopes, unique safe ids, and — critically — that the declared
// path scopes are pairwise disjoint so the merge-back cannot conflict.
func ParseWorkerPoolArgs(args map[string]any, maxTasks int) (WorkerPoolArgs, error) {
	raw, ok := args["tasks"].([]any)
	if !ok || len(raw) == 0 {
		return WorkerPoolArgs{}, fmt.Errorf("%w: tasks must be a non-empty array", ErrToolValidation)
	}
	if maxTasks > 0 && len(raw) > maxTasks {
		return WorkerPoolArgs{}, fmt.Errorf("%w: tasks has %d entries, exceeding the limit of %d",
			ErrToolValidation, len(raw), maxTasks)
	}

	tasks := make([]SubTask, 0, len(raw))
	seenID := make(map[string]struct{}, len(raw))
	for i, elem := range raw {
		m, ok := elem.(map[string]any)
		if !ok {
			return WorkerPoolArgs{}, fmt.Errorf("%w: tasks[%d] must be an object", ErrToolValidation, i)
		}
		prompt, _ := m["prompt"].(string)
		if strings.TrimSpace(prompt) == "" {
			return WorkerPoolArgs{}, fmt.Errorf("%w: tasks[%d].prompt must be a non-empty string", ErrToolValidation, i)
		}
		if len(prompt) > maxSubTaskPromptBytes {
			return WorkerPoolArgs{}, fmt.Errorf("%w: tasks[%d].prompt exceeds %d bytes", ErrToolValidation, i, maxSubTaskPromptBytes)
		}
		paths, err := parseTaskPaths(m["paths"], i)
		if err != nil {
			return WorkerPoolArgs{}, err
		}
		id, _ := m["id"].(string)
		id = strings.TrimSpace(id)
		if id == "" {
			id = fmt.Sprintf("task%d", i+1)
		}
		if err := validateTaskID(id); err != nil {
			return WorkerPoolArgs{}, err
		}
		if _, dup := seenID[id]; dup {
			return WorkerPoolArgs{}, fmt.Errorf("%w: duplicate task id %q", ErrToolValidation, id)
		}
		seenID[id] = struct{}{}
		tasks = append(tasks, SubTask{ID: id, Prompt: prompt, Paths: paths})
	}

	for i := 0; i < len(tasks); i++ {
		for j := i + 1; j < len(tasks); j++ {
			if p, q, overlap := scopesOverlap(tasks[i].Paths, tasks[j].Paths); overlap {
				return WorkerPoolArgs{}, fmt.Errorf(
					"%w: tasks %q and %q claim overlapping paths (%q vs %q); each file may be owned by only one sub-task",
					ErrToolValidation, tasks[i].ID, tasks[j].ID, p, q)
			}
		}
	}
	return WorkerPoolArgs{Tasks: tasks}, nil
}

// parseTaskPaths decodes and cleans one task's `paths` array.
func parseTaskPaths(v any, taskIdx int) ([]string, error) {
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		return nil, fmt.Errorf("%w: tasks[%d].paths must be a non-empty array", ErrToolValidation, taskIdx)
	}
	out := make([]string, 0, len(list))
	for _, raw := range list {
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("%w: tasks[%d].paths must be a list of strings", ErrToolValidation, taskIdx)
		}
		cleaned, err := cleanScopePath(s)
		if err != nil {
			return nil, err
		}
		out = append(out, cleaned)
	}
	return out, nil
}

// validateTaskID rejects ids that would be unsafe as a path segment or git
// branch component.
func validateTaskID(id string) error {
	if len(id) > 64 {
		return fmt.Errorf("%w: task id %q exceeds 64 chars", ErrToolValidation, id)
	}
	if strings.Contains(id, "..") {
		return fmt.Errorf("%w: task id %q must not contain '..'", ErrToolValidation, id)
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-':
		default:
			return fmt.Errorf("%w: task id %q may only contain letters, digits, '.', '_', '-'", ErrToolValidation, id)
		}
	}
	return nil
}

// cleanScopePath normalizes a declared scope entry and rejects absolute paths,
// '..' traversal, and the bare workspace root.
func cleanScopePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("%w: a paths entry is empty", ErrToolValidation)
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("%w: path %q must be relative to the workspace root", ErrToolValidation, p)
	}
	c := path.Clean(p)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("%w: path %q must name a file or directory inside the workspace", ErrToolValidation, p)
	}
	return c, nil
}

// scopesOverlap reports the first pair of conflicting paths between two
// declared scopes. Two paths conflict when they are equal or one is an
// ancestor directory of the other.
func scopesOverlap(a, b []string) (string, string, bool) {
	for _, x := range a {
		for _, y := range b {
			if x == y || strings.HasPrefix(y, x+"/") || strings.HasPrefix(x, y+"/") {
				return x, y, true
			}
		}
	}
	return "", "", false
}

// PathInScope reports whether a cleaned, workspace-relative path falls within
// one of the declared scope entries (the entry itself, or a file beneath an
// entry that names a directory).
func PathInScope(p string, scope []string) bool {
	for _, s := range scope {
		if p == s || strings.HasPrefix(p, s+"/") {
			return true
		}
	}
	return false
}

// CleanWorkspacePath normalizes a tool's workspace-relative path argument,
// rejecting absolute paths and '..' traversal. Unlike cleanScopePath it allows
// "." (used by list_dir to mean the worktree root).
func CleanWorkspacePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("%w: path must not be empty", ErrToolValidation)
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("%w: path %q must be relative to the workspace root", ErrToolValidation, p)
	}
	c := path.Clean(p)
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("%w: path %q escapes the workspace root", ErrToolValidation, p)
	}
	return c, nil
}
