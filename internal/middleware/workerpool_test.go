package middleware

import (
	"errors"
	"testing"
)

func TestParseWorkerPoolArgs_Valid(t *testing.T) {
	args := map[string]any{
		"tasks": []any{
			map[string]any{"id": "auth", "prompt": "migrate auth", "paths": []any{"internal/auth"}},
			map[string]any{"prompt": "migrate api", "paths": []any{"internal/api/handlers.go", "internal/api/routes.go"}},
		},
	}
	got, err := ParseWorkerPoolArgs(args, 8)
	if err != nil {
		t.Fatalf("ParseWorkerPoolArgs: %v", err)
	}
	if len(got.Tasks) != 2 {
		t.Fatalf("tasks = %d, want 2", len(got.Tasks))
	}
	if got.Tasks[0].ID != "auth" {
		t.Errorf("task0 id = %q, want auth", got.Tasks[0].ID)
	}
	// Second task had no id — one is auto-assigned.
	if got.Tasks[1].ID != "task2" {
		t.Errorf("task1 id = %q, want auto-assigned task2", got.Tasks[1].ID)
	}
}

func TestParseWorkerPoolArgs_Invalid(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		max  int
	}{
		{"no tasks key", map[string]any{}, 8},
		{"empty tasks", map[string]any{"tasks": []any{}}, 8},
		{
			"too many tasks",
			map[string]any{"tasks": []any{
				map[string]any{"prompt": "a", "paths": []any{"a"}},
				map[string]any{"prompt": "b", "paths": []any{"b"}},
			}},
			1,
		},
		{
			"empty prompt",
			map[string]any{"tasks": []any{map[string]any{"prompt": "  ", "paths": []any{"a"}}}},
			8,
		},
		{
			"missing paths",
			map[string]any{"tasks": []any{map[string]any{"prompt": "go"}}},
			8,
		},
		{
			"empty paths array",
			map[string]any{"tasks": []any{map[string]any{"prompt": "go", "paths": []any{}}}},
			8,
		},
		{
			"absolute path",
			map[string]any{"tasks": []any{map[string]any{"prompt": "go", "paths": []any{"/etc/passwd"}}}},
			8,
		},
		{
			"traversal path",
			map[string]any{"tasks": []any{map[string]any{"prompt": "go", "paths": []any{"../escape"}}}},
			8,
		},
		{
			"duplicate id",
			map[string]any{"tasks": []any{
				map[string]any{"id": "x", "prompt": "a", "paths": []any{"a"}},
				map[string]any{"id": "x", "prompt": "b", "paths": []any{"b"}},
			}},
			8,
		},
		{
			"unsafe id",
			map[string]any{"tasks": []any{map[string]any{"id": "../x", "prompt": "a", "paths": []any{"a"}}}},
			8,
		},
		{
			"overlapping scopes — identical",
			map[string]any{"tasks": []any{
				map[string]any{"id": "a", "prompt": "a", "paths": []any{"pkg/x.go"}},
				map[string]any{"id": "b", "prompt": "b", "paths": []any{"pkg/x.go"}},
			}},
			8,
		},
		{
			"overlapping scopes — dir contains file",
			map[string]any{"tasks": []any{
				map[string]any{"id": "a", "prompt": "a", "paths": []any{"pkg"}},
				map[string]any{"id": "b", "prompt": "b", "paths": []any{"pkg/x.go"}},
			}},
			8,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseWorkerPoolArgs(tc.args, tc.max); err == nil {
				t.Fatalf("expected error, got nil")
			} else if !errors.Is(err, ErrToolValidation) {
				t.Errorf("error %v is not ErrToolValidation", err)
			}
		})
	}
}

func TestParseWorkerPoolArgs_DisjointDirsAccepted(t *testing.T) {
	// Sibling directories that share a prefix string but are not nested
	// (pkg/a vs pkg/ab) must NOT be treated as overlapping.
	args := map[string]any{"tasks": []any{
		map[string]any{"id": "a", "prompt": "a", "paths": []any{"pkg/a"}},
		map[string]any{"id": "b", "prompt": "b", "paths": []any{"pkg/ab"}},
	}}
	if _, err := ParseWorkerPoolArgs(args, 8); err != nil {
		t.Fatalf("sibling dirs should be disjoint: %v", err)
	}
}

func TestSubDispatcherTools_ExcludesWorkerPool(t *testing.T) {
	all := append(DefaultTools(), WorkerPoolSpec())
	sub := SubDispatcherTools(all)
	if len(sub) != len(all)-1 {
		t.Fatalf("sub catalogue len = %d, want %d", len(sub), len(all)-1)
	}
	for _, tool := range sub {
		if tool.Name == ToolDispatchWorkerPool {
			t.Fatal("dispatch_worker_pool must not appear in a sub-dispatcher catalogue (fork-bomb guard)")
		}
	}
	// Every other tool survives.
	if len(SubDispatcherTools(DefaultTools())) != len(DefaultTools()) {
		t.Error("SubDispatcherTools dropped a non-pool tool")
	}
}

func TestWorkerPool_IsMutatingAndKnown(t *testing.T) {
	if !IsMutatingBaseTool(ToolDispatchWorkerPool) {
		t.Error("dispatch_worker_pool must be classified as mutating (it merges into the primary branch)")
	}
	if !KnownTool(ToolDispatchWorkerPool) {
		t.Error("dispatch_worker_pool must be a known tool")
	}
}

func TestWorkerPoolSpec_Shape(t *testing.T) {
	spec := WorkerPoolSpec()
	if spec.Name != ToolDispatchWorkerPool {
		t.Fatalf("spec name = %q", spec.Name)
	}
	tasks := spec.Parameters.Properties["tasks"]
	if tasks == nil || tasks.Type != "array" || tasks.Items == nil {
		t.Fatalf("tasks property malformed: %+v", tasks)
	}
	required := tasks.Items.Required
	wantPrompt, wantPaths := false, false
	for _, r := range required {
		if r == "prompt" {
			wantPrompt = true
		}
		if r == "paths" {
			wantPaths = true
		}
	}
	if !wantPrompt || !wantPaths {
		t.Errorf("task item required = %v, want prompt+paths", required)
	}
}

func TestPathInScope(t *testing.T) {
	scope := []string{"internal/auth", "cmd/main.go"}
	in := []string{"internal/auth", "internal/auth/jwt.go", "internal/auth/sub/x.go", "cmd/main.go"}
	for _, p := range in {
		if !PathInScope(p, scope) {
			t.Errorf("PathInScope(%q) = false, want true", p)
		}
	}
	out := []string{"internal/authx", "internal/api/x.go", "cmd/main.go.bak", "other"}
	for _, p := range out {
		if PathInScope(p, scope) {
			t.Errorf("PathInScope(%q) = true, want false", p)
		}
	}
}

func TestCleanWorkspacePath(t *testing.T) {
	ok := map[string]string{
		"a/b.go":   "a/b.go",
		"./a/b.go": "a/b.go",
		".":        ".",
		"a//b":     "a/b",
	}
	for in, want := range ok {
		got, err := CleanWorkspacePath(in)
		if err != nil {
			t.Errorf("CleanWorkspacePath(%q) error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("CleanWorkspacePath(%q) = %q, want %q", in, got, want)
		}
	}
	bad := []string{"", "  ", "/abs", "../up", "a/../../up"}
	for _, in := range bad {
		if _, err := CleanWorkspacePath(in); err == nil {
			t.Errorf("CleanWorkspacePath(%q) = nil error, want rejection", in)
		}
	}
}

func TestValidate_DispatchWorkerPool(t *testing.T) {
	good := map[string]any{"tasks": []any{
		map[string]any{"prompt": "go", "paths": []any{"x"}},
	}}
	if err := Validate(ToolDispatchWorkerPool, good); err != nil {
		t.Errorf("Validate(good) = %v", err)
	}
	if err := Validate(ToolDispatchWorkerPool, map[string]any{}); err == nil {
		t.Error("Validate(empty) = nil, want error")
	}
}
