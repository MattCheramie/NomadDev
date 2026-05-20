package middleware

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mattcheramie/nomaddev/internal/history"
	"github.com/mattcheramie/nomaddev/internal/sandbox"
)

// fakeGitHubCaller satisfies middleware.GitHubCaller for factory + dispatcher
// tests. Records every Call invocation so tests can assert routing.
type fakeGitHubCaller struct {
	calls []ToolCall
}

func (f *fakeGitHubCaller) Call(_ context.Context, call ToolCall, _ DispatchOptions) (<-chan sandbox.ExecChunk, error) {
	f.calls = append(f.calls, call)
	ch := make(chan sandbox.ExecChunk, 2)
	ch <- sandbox.ExecChunk{Stream: sandbox.StreamStdout, Data: []byte(`{"ok":true}`)}
	ch <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: 0}
	close(ch)
	return ch, nil
}

func TestFactory_NoneReturnsNil(t *testing.T) {
	svc, err := NewService(context.Background(), FactoryConfig{Runtime: ""})
	if err != nil || svc != nil {
		t.Fatalf("Runtime=\"\": want (nil, nil), got (%v, %v)", svc, err)
	}
	svc, err = NewService(context.Background(), FactoryConfig{Runtime: RuntimeNone})
	if err != nil || svc != nil {
		t.Fatalf("Runtime=none: want (nil, nil), got (%v, %v)", svc, err)
	}
}

func TestFactory_MockReturnsService(t *testing.T) {
	svc, err := NewService(context.Background(), FactoryConfig{
		Runtime: RuntimeMock,
		History: history.NewMemoryStore(),
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if svc == nil || svc.Translator == nil || svc.Approver == nil || svc.Dispatcher == nil || svc.History == nil {
		t.Fatalf("Service partially built: %+v", svc)
	}
}

func TestFactory_MockRequiresHistory(t *testing.T) {
	_, err := NewService(context.Background(), FactoryConfig{Runtime: RuntimeMock})
	if err == nil || !strings.Contains(err.Error(), "history") {
		t.Fatalf("want history error, got %v", err)
	}
}

func TestFactory_UnknownReturnsError(t *testing.T) {
	_, err := NewService(context.Background(), FactoryConfig{Runtime: "qemu"})
	if err == nil || !strings.Contains(err.Error(), "qemu") {
		t.Fatalf("want unknown-runtime error, got %v", err)
	}
}

func TestFactory_GitHubBackend_WiresDispatcherToolsAndApproval(t *testing.T) {
	caller := &fakeGitHubCaller{}
	tools := []ToolSpec{
		{Name: "github_list_repositories", Description: "ro"},
		{Name: "github_create_issue", Description: "mut"},
		{Name: "github_create_pull_request", Description: "mut"},
	}
	destructive := func(name string) bool {
		return name == "github_create_issue" || name == "github_create_pull_request"
	}

	svc, err := NewService(context.Background(), FactoryConfig{
		Runtime:                 RuntimeMock,
		History:                 history.NewMemoryStore(),
		GitHub:                  caller,
		GitHubTools:             tools,
		IsDestructiveGitHubTool: destructive,
		ApprovalRequiredTools:   []string{ToolExecuteScript},
		ApprovalTimeout:         time.Second,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// Tools merged: DefaultTools (8) + github tools (3).
	if got := len(svc.AvailableTools()); got != 11 {
		t.Fatalf("AvailableTools count = %d, want 11", got)
	}

	// Dispatcher routes github_* to the fake caller.
	cd, ok := svc.Dispatcher.(*CompositeDispatcher)
	if !ok {
		t.Fatalf("dispatcher type = %T, want *CompositeDispatcher", svc.Dispatcher)
	}
	if cd.GitHub == nil {
		t.Fatal("dispatcher.GitHub not wired")
	}
	ch, err := cd.Dispatch(context.Background(), ToolCall{Tool: "github_list_repositories"}, DispatchOptions{})
	if err != nil {
		t.Fatalf("Dispatch routing: %v", err)
	}
	// Drain.
	for range ch {
	}
	if len(caller.calls) != 1 || caller.calls[0].Tool != "github_list_repositories" {
		t.Fatalf("routing recorded = %+v", caller.calls)
	}

	// Approval auto-populated for destructive tools only.
	pa, ok := svc.Approver.(*PolicyApprover)
	if !ok {
		t.Fatalf("approver type = %T", svc.Approver)
	}
	if req, _ := pa.RequiresApproval("github_create_issue", nil); !req {
		t.Error("github_create_issue not auto-gated")
	}
	if req, _ := pa.RequiresApproval("github_create_pull_request", nil); !req {
		t.Error("github_create_pull_request not auto-gated")
	}
	if req, _ := pa.RequiresApproval("github_list_repositories", nil); req {
		t.Error("github_list_repositories should NOT be gated (read-only)")
	}
	// Existing required tool still gated.
	if req, _ := pa.RequiresApproval(ToolExecuteScript, nil); !req {
		t.Error("execute_script lost its gating after GitHub wiring")
	}
}

func TestFactory_NoGitHub_DefaultsPreserved(t *testing.T) {
	svc, err := NewService(context.Background(), FactoryConfig{
		Runtime: RuntimeMock,
		History: history.NewMemoryStore(),
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if got := len(svc.AvailableTools()); got != 8 {
		t.Fatalf("AvailableTools count = %d, want 8 (DefaultTools)", got)
	}
	cd := svc.Dispatcher.(*CompositeDispatcher)
	if cd.GitHub != nil {
		t.Fatal("GitHub should be nil when not wired")
	}

	// Dispatch of github_* tool returns ErrBadRequest (not configured).
	_, err = cd.Dispatch(context.Background(), ToolCall{Tool: "github_anything"}, DispatchOptions{})
	if err == nil {
		t.Fatal("want error for github_* when GitHub backend not wired")
	}
}

// fakeSandboxRunner satisfies sandbox.Runner and records the ExecRequest
// it receives, so the dispatcher routing test can assert that
// search_syntax is forwarded with MaxResultBytes plumbed through.
type fakeSandboxRunner struct {
	last sandbox.ExecRequest
}

func (f *fakeSandboxRunner) Exec(_ context.Context, req sandbox.ExecRequest) (<-chan sandbox.ExecChunk, error) {
	f.last = req
	ch := make(chan sandbox.ExecChunk, 2)
	ch <- sandbox.ExecChunk{Stream: sandbox.StreamStdout, Data: []byte(`{"matches":[]}`)}
	ch <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: 0}
	close(ch)
	return ch, nil
}

func TestDispatcher_RoutesSearchSyntaxToSandbox(t *testing.T) {
	runner := &fakeSandboxRunner{}
	d := NewCompositeDispatcher(runner, nil)
	ch, err := d.Dispatch(context.Background(),
		ToolCall{Tool: ToolSearchSyntax, Args: map[string]any{"pattern": "$X"}},
		DispatchOptions{MaxResultBytes: 4096})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	for range ch {
	}
	if runner.last.Tool != ToolSearchSyntax {
		t.Errorf("runner.last.Tool = %q, want %q", runner.last.Tool, ToolSearchSyntax)
	}
	if runner.last.MaxResultBytes != 4096 {
		t.Errorf("runner.last.MaxResultBytes = %d, want 4096", runner.last.MaxResultBytes)
	}
}

func TestFactory_WiresReferenceBuffer(t *testing.T) {
	svc, err := NewService(context.Background(), FactoryConfig{
		Runtime: RuntimeMock,
		History: history.NewMemoryStore(),
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if svc.Pins == nil {
		t.Fatal("Service.Pins not constructed")
	}
	cd, ok := svc.Dispatcher.(*CompositeDispatcher)
	if !ok {
		t.Fatalf("dispatcher type = %T, want *CompositeDispatcher", svc.Dispatcher)
	}
	if cd.Pins != svc.Pins {
		t.Fatal("dispatcher and Service hold different reference buffers")
	}
}

func TestFactory_PlumbsMaxResultBytesToService(t *testing.T) {
	svc, err := NewService(context.Background(), FactoryConfig{
		Runtime:        RuntimeMock,
		History:        history.NewMemoryStore(),
		MaxResultBytes: 12345,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if svc.Config.MaxResultBytes != 12345 {
		t.Errorf("Service.Config.MaxResultBytes = %d, want 12345", svc.Config.MaxResultBytes)
	}
}

func TestFactory_GeminiWithoutTagReturnsError(t *testing.T) {
	svc, err := NewService(context.Background(), FactoryConfig{
		Runtime: RuntimeGemini,
		History: history.NewMemoryStore(),
	})
	if err == nil && svc != nil {
		t.Skip("built with -tags gemini; stub test does not apply")
	}
	if err == nil {
		t.Fatal("expected error for gemini runtime without -tags gemini")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "gemini") {
		t.Errorf("error should mention gemini, got %v", err)
	}
}

// TestFactory_OpenAIFamilyWithoutTag exercises the openai and deepseek
// runtime selectors. Both route through newOpenAITranslator and so share
// the stub error when the binary lacks -tags openai. With the tag present
// and an APIKey supplied they would each return a real translator; here we
// just confirm dispatch (and skip silently when built with the tag).
func TestFactory_OpenAIFamilyWithoutTag(t *testing.T) {
	for _, rt := range []string{RuntimeOpenAI, RuntimeDeepSeek} {
		t.Run(rt, func(t *testing.T) {
			svc, err := NewService(context.Background(), FactoryConfig{
				Runtime: rt,
				APIKey:  "test-key", // bypass the explicit empty-key error path
				History: history.NewMemoryStore(),
			})
			if err == nil && svc != nil {
				t.Skip("built with -tags openai; stub test does not apply")
			}
			if err == nil {
				t.Fatalf("expected error for runtime=%q without -tags openai", rt)
			}
			if !strings.Contains(strings.ToLower(err.Error()), "openai") {
				t.Errorf("error should mention openai (the shared stub), got %v", err)
			}
		})
	}
}

func TestFactory_AnthropicWithoutTagReturnsError(t *testing.T) {
	svc, err := NewService(context.Background(), FactoryConfig{
		Runtime: RuntimeAnthropic,
		APIKey:  "test-key",
		History: history.NewMemoryStore(),
	})
	if err == nil && svc != nil {
		t.Skip("built with -tags anthropic; stub test does not apply")
	}
	if err == nil {
		t.Fatal("expected error for anthropic runtime without -tags anthropic")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "anthropic") {
		t.Errorf("error should mention anthropic, got %v", err)
	}
}
