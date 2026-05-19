package middleware

import (
	"context"
	"errors"
	"testing"

	"github.com/mattcheramie/nomaddev/internal/sandbox"
)

func TestDispatcher_AuditMode_RefusesMutatingBaseTools(t *testing.T) {
	d := &CompositeDispatcher{}
	for _, tool := range []string{ToolExecuteScript, ToolWritePatch, ToolApplyCodePatch} {
		_, err := d.Dispatch(context.Background(),
			ToolCall{ID: "c1", Tool: tool, Args: map[string]any{}},
			DispatchOptions{Mode: ModeAudit},
		)
		if err == nil {
			t.Fatalf("Dispatch(%q, audit) succeeded, want refusal", tool)
		}
		if !errors.Is(err, sandbox.ErrBadRequest) {
			t.Fatalf("Dispatch(%q, audit) err = %v, want ErrBadRequest", tool, err)
		}
	}
}

func TestDispatcher_AuditMode_AllowsReadOnlyBaseTools(t *testing.T) {
	// Read-only base tools should still reach their backends — and fail
	// with the "backend not configured" error here, not with the audit
	// rejection. That proves the audit gate is mutator-scoped.
	d := &CompositeDispatcher{}
	for _, tool := range []string{ToolReadFile, ToolListDir, ToolSearchSyntax} {
		_, err := d.Dispatch(context.Background(),
			ToolCall{ID: "c1", Tool: tool, Args: map[string]any{}},
			DispatchOptions{Mode: ModeAudit},
		)
		if err == nil {
			t.Fatalf("Dispatch(%q): want backend-missing error, got nil", tool)
		}
		// Sanity: same error class either way (audit refusal also wraps
		// ErrBadRequest), so we check the message *doesn't* mention audit.
		if msg := err.Error(); contains(msg, "audit mode") {
			t.Fatalf("Dispatch(%q, audit): audit gate fired on read-only tool: %v", tool, err)
		}
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
