package middleware

import (
	"errors"
	"strings"
	"testing"
)

func TestTools_DefaultTools_AllEntries(t *testing.T) {
	specs := DefaultTools()
	if len(specs) != 6 {
		t.Fatalf("want 6 tools, got %d", len(specs))
	}
	seen := map[string]bool{}
	for _, s := range specs {
		seen[s.Name] = true
	}
	for _, want := range []string{
		ToolExecuteScript, ToolReadFile, ToolListDir,
		ToolWritePatch, ToolApplyCodePatch, ToolSearchSyntax,
	} {
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

func TestValidate_ApplyCodePatch_VerifyCommand_OK(t *testing.T) {
	if err := Validate(ToolApplyCodePatch, map[string]any{
		"file_path":      "x.go",
		"search_string":  "old",
		"replace_string": "new",
		"verify_command": "go build ./...",
	}); err != nil {
		t.Fatalf("Validate with verify_command: %v", err)
	}
}

func TestValidate_ApplyCodePatch_VerifyCommand_RejectNonString(t *testing.T) {
	err := Validate(ToolApplyCodePatch, map[string]any{
		"file_path":      "x.go",
		"search_string":  "old",
		"replace_string": "new",
		"verify_command": 42,
	})
	if !errors.Is(err, ErrToolValidation) {
		t.Fatalf("want ErrToolValidation, got %v", err)
	}
}

func TestValidate_ApplyCodePatch_VerifyCommand_RejectOversize(t *testing.T) {
	err := Validate(ToolApplyCodePatch, map[string]any{
		"file_path":      "x.go",
		"search_string":  "old",
		"replace_string": "new",
		"verify_command": strings.Repeat("a", 8*1024+1),
	})
	if !errors.Is(err, ErrToolValidation) {
		t.Fatalf("want ErrToolValidation, got %v", err)
	}
}

func TestValidate_SearchSyntax_OK(t *testing.T) {
	cases := []map[string]any{
		{"pattern": "fn $F($_: context.Context)"},
		{"pattern": "$X", "lang": "go"},
		{"pattern": "$X", "lang": "go", "path": "internal/middleware"},
		{"pattern": "$X", "max_matches": 50},
		{"pattern": "$X", "max_matches": float64(50)}, // JSON-decoded shape
		{"pattern": "$X", "globs": []any{"*.go", "!*_test.go"}},
		{"pattern": "$X", "globs": []string{"*.go"}},
	}
	for i, args := range cases {
		if err := Validate(ToolSearchSyntax, args); err != nil {
			t.Errorf("case %d: Validate: %v", i, err)
		}
	}
}

func TestValidate_SearchSyntax_RejectMissingPattern(t *testing.T) {
	err := Validate(ToolSearchSyntax, map[string]any{"lang": "go"})
	if !errors.Is(err, ErrToolValidation) {
		t.Fatalf("want ErrToolValidation, got %v", err)
	}
}

func TestValidate_SearchSyntax_RejectOversizePattern(t *testing.T) {
	err := Validate(ToolSearchSyntax, map[string]any{"pattern": strings.Repeat("a", 8*1024+1)})
	if !errors.Is(err, ErrToolValidation) {
		t.Fatalf("want ErrToolValidation, got %v", err)
	}
}

func TestValidate_SearchSyntax_RejectAbsolutePath(t *testing.T) {
	err := Validate(ToolSearchSyntax, map[string]any{"pattern": "$X", "path": "/etc/passwd"})
	if !errors.Is(err, ErrToolValidation) {
		t.Fatalf("want ErrToolValidation, got %v", err)
	}
}

func TestValidate_SearchSyntax_RejectDotDot(t *testing.T) {
	for _, p := range []string{"..", "../etc", "sub/../../etc"} {
		err := Validate(ToolSearchSyntax, map[string]any{"pattern": "$X", "path": p})
		if !errors.Is(err, ErrToolValidation) {
			t.Errorf("path %q: want ErrToolValidation, got %v", p, err)
		}
	}
}

func TestValidate_SearchSyntax_RejectBadLang(t *testing.T) {
	for _, l := range []string{"go-1.21", "py3", "lang with space"} {
		err := Validate(ToolSearchSyntax, map[string]any{"pattern": "$X", "lang": l})
		if !errors.Is(err, ErrToolValidation) {
			t.Errorf("lang %q: want ErrToolValidation, got %v", l, err)
		}
	}
}

func TestValidate_SearchSyntax_RejectBadMaxMatches(t *testing.T) {
	for i, args := range []map[string]any{
		{"pattern": "$X", "max_matches": 0},
		{"pattern": "$X", "max_matches": 1001},
		{"pattern": "$X", "max_matches": "many"},
	} {
		if err := Validate(ToolSearchSyntax, args); !errors.Is(err, ErrToolValidation) {
			t.Errorf("case %d: want ErrToolValidation, got %v", i, err)
		}
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
