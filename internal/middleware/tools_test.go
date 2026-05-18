package middleware

import (
	"errors"
	"strings"
	"testing"
)

func TestTools_DefaultTools_AllEntries(t *testing.T) {
	specs := DefaultTools()
	if len(specs) != 5 {
		t.Fatalf("want 5 tools, got %d", len(specs))
	}
	seen := map[string]bool{}
	for _, s := range specs {
		seen[s.Name] = true
	}
	for _, want := range []string{ToolExecuteScript, ToolReadFile, ToolListDir, ToolWritePatch, ToolApplyCodePatch} {
		if !seen[want] {
			t.Errorf("DefaultTools missing %q", want)
		}
	}
}

func TestTools_KnownTool(t *testing.T) {
	if !KnownTool(ToolExecuteScript) || !KnownTool(ToolWritePatch) {
		t.Fatal("known tools rejected")
	}
	if KnownTool("nope") {
		t.Fatal("unknown tool accepted")
	}
}

func TestValidate_ExecuteScript_OK(t *testing.T) {
	if err := Validate(ToolExecuteScript, map[string]any{"script": "echo hi"}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidate_ExecuteScript_MissingScript(t *testing.T) {
	err := Validate(ToolExecuteScript, map[string]any{})
	if !errors.Is(err, ErrToolValidation) {
		t.Fatalf("want ErrToolValidation, got %v", err)
	}
}

func TestValidate_ExecuteScript_RejectOversizeScript(t *testing.T) {
	big := strings.Repeat("a", 64*1024+1)
	err := Validate(ToolExecuteScript, map[string]any{"script": big})
	if !errors.Is(err, ErrToolValidation) {
		t.Fatalf("want ErrToolValidation, got %v", err)
	}
}

func TestValidate_ReadFile_OK(t *testing.T) {
	if err := Validate(ToolReadFile, map[string]any{"path": "x.txt"}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidate_ReadFile_MissingPath(t *testing.T) {
	err := Validate(ToolReadFile, map[string]any{})
	if !errors.Is(err, ErrToolValidation) {
		t.Fatalf("want ErrToolValidation, got %v", err)
	}
}

func TestValidate_ListDir_OK(t *testing.T) {
	if err := Validate(ToolListDir, map[string]any{"path": "sub"}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidate_WritePatch_OK(t *testing.T) {
	if err := Validate(ToolWritePatch, map[string]any{"path": "x.txt", "content": "hi"}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidate_WritePatch_RejectMissingContent(t *testing.T) {
	err := Validate(ToolWritePatch, map[string]any{"path": "x.txt"})
	if !errors.Is(err, ErrToolValidation) {
		t.Fatalf("want ErrToolValidation, got %v", err)
	}
}

func TestValidate_WritePatch_RejectBadMode(t *testing.T) {
	err := Validate(ToolWritePatch, map[string]any{
		"path": "x.txt", "content": "hi", "mode": "rewrite",
	})
	if !errors.Is(err, ErrToolValidation) {
		t.Fatalf("want ErrToolValidation, got %v", err)
	}
}

func TestValidate_ApplyCodePatch_OK(t *testing.T) {
	if err := Validate(ToolApplyCodePatch, map[string]any{
		"file_path":      "x.go",
		"search_string":  "old",
		"replace_string": "new",
	}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidate_ApplyCodePatch_AllowsEmptyReplace(t *testing.T) {
	// replace_string="" is a valid pure deletion.
	if err := Validate(ToolApplyCodePatch, map[string]any{
		"file_path":      "x.go",
		"search_string":  "old",
		"replace_string": "",
	}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidate_ApplyCodePatch_RejectMissingFields(t *testing.T) {
	cases := []map[string]any{
		{"search_string": "a", "replace_string": "b"},
		{"file_path": "x", "replace_string": "b"},
		{"file_path": "x", "search_string": "a"},
	}
	for i, args := range cases {
		if err := Validate(ToolApplyCodePatch, args); !errors.Is(err, ErrToolValidation) {
			t.Errorf("case %d: want ErrToolValidation, got %v", i, err)
		}
	}
}

func TestValidate_ApplyCodePatch_RejectNonStringReplace(t *testing.T) {
	err := Validate(ToolApplyCodePatch, map[string]any{
		"file_path":      "x",
		"search_string":  "a",
		"replace_string": 42,
	})
	if !errors.Is(err, ErrToolValidation) {
		t.Fatalf("want ErrToolValidation, got %v", err)
	}
}

func TestValidate_UnknownTool(t *testing.T) {
	if err := Validate("nope", map[string]any{}); !errors.Is(err, ErrToolValidation) {
		t.Fatalf("want ErrToolValidation, got %v", err)
	}
}

func TestKnownTool_AcceptsGitHubPrefix(t *testing.T) {
	if !KnownTool("github_list_repositories") {
		t.Fatal("github_* tool rejected by KnownTool")
	}
	if !KnownTool("github_create_pull_request") {
		t.Fatal("github_* tool rejected by KnownTool")
	}
}

func TestValidate_GitHubPrefix_Passthrough(t *testing.T) {
	// Arg validation is delegated to the upstream MCP server; the middleware
	// layer only does a prefix check so unknown github_* names are accepted.
	if err := Validate("github_list_repositories", map[string]any{"foo": "bar"}); err != nil {
		t.Fatalf("github_* passthrough returned %v, want nil", err)
	}
	if err := Validate("github_create_issue", nil); err != nil {
		t.Fatalf("github_* nil-args passthrough returned %v, want nil", err)
	}
}
