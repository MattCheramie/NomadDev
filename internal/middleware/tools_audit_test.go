package middleware

import (
	"sort"
	"testing"
)

func TestAuditMode_IsMutatingBaseTool(t *testing.T) {
	mutating := []string{ToolExecuteScript, ToolWritePatch, ToolApplyCodePatch}
	for _, name := range mutating {
		if !IsMutatingBaseTool(name) {
			t.Errorf("IsMutatingBaseTool(%q) = false, want true", name)
		}
	}
	readOnly := []string{ToolReadFile, ToolListDir, ToolSearchSyntax, ToolPinFile, ToolUnpinFile}
	for _, name := range readOnly {
		if IsMutatingBaseTool(name) {
			t.Errorf("IsMutatingBaseTool(%q) = true, want false", name)
		}
	}
	if IsMutatingBaseTool("github_create_issue") {
		t.Error("base classifier should not infer github_* tools")
	}
}

func TestAuditMode_AvailableToolsFor_StripsMutators(t *testing.T) {
	svc := &Service{
		Tools: append(DefaultTools(),
			ToolSpec{Name: "github_get_file_contents"},
			ToolSpec{Name: "github_create_issue"},
		),
		IsDestructiveGitHubTool: func(name string) bool {
			return name == "github_create_issue"
		},
	}

	got := svc.AvailableToolsFor(ModeAudit)
	gotNames := make([]string, 0, len(got))
	for _, s := range got {
		gotNames = append(gotNames, s.Name)
	}
	sort.Strings(gotNames)

	want := []string{
		"github_get_file_contents",
		ToolFetchExternalDocs,
		ToolListDir,
		ToolPinFile,
		ToolReadFile,
		ToolSearchSyntax,
		ToolUnpinFile,
	}
	sort.Strings(want)
	if len(gotNames) != len(want) {
		t.Fatalf("AvailableToolsFor(audit): got %v, want %v", gotNames, want)
	}
	for i := range want {
		if gotNames[i] != want[i] {
			t.Fatalf("AvailableToolsFor(audit): got %v, want %v", gotNames, want)
		}
	}
}

func TestAuditMode_AvailableToolsFor_NormalUnchanged(t *testing.T) {
	svc := &Service{Tools: DefaultTools()}
	got := svc.AvailableToolsFor(ModeNormal)
	if len(got) != len(DefaultTools()) {
		t.Fatalf("normal mode should not filter: got %d, want %d", len(got), len(DefaultTools()))
	}
}

func TestAuditMode_IsMutatingTool_GitHub(t *testing.T) {
	svc := &Service{
		IsDestructiveGitHubTool: func(name string) bool {
			return name == "github_create_issue"
		},
	}
	if !svc.IsMutatingTool("github_create_issue") {
		t.Error("github_create_issue should classify as mutating")
	}
	if svc.IsMutatingTool("github_get_file_contents") {
		t.Error("github_get_file_contents should classify as read-only")
	}
	// No predicate wired: github_* falls through to non-mutating.
	bare := &Service{}
	if bare.IsMutatingTool("github_create_issue") {
		t.Error("without predicate, github_* should not be classified as mutating")
	}
}
