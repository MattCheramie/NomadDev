package middleware

import (
	"errors"
	"strings"
	"testing"
)

func TestTools_DefaultTools_FourEntries(t *testing.T) {
	specs := DefaultTools()
	if len(specs) != 4 {
		t.Fatalf("want 4 tools, got %d", len(specs))
	}
	seen := map[string]bool{}
	for _, s := range specs {
		seen[s.Name] = true
	}
	for _, want := range []string{ToolExecuteScript, ToolReadFile, ToolListDir, ToolWritePatch} {
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

func TestValidate_UnknownTool(t *testing.T) {
	if err := Validate("nope", map[string]any{}); !errors.Is(err, ErrToolValidation) {
		t.Fatalf("want ErrToolValidation, got %v", err)
	}
}
