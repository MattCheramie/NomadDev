package middleware

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/mattcheramie/nomaddev/internal/fsops"
	"github.com/mattcheramie/nomaddev/internal/history"
	"github.com/mattcheramie/nomaddev/internal/sandbox"
)

// Runtime selectors recognized by NewService.
const (
	RuntimeNone   = "none"
	RuntimeMock   = "mock"
	RuntimeGemini = "gemini"
)

// FactoryConfig is the runtime-agnostic configuration for NewService.
type FactoryConfig struct {
	Runtime string

	// Translator backends.
	APIKey      string
	Model       string
	Temperature float64
	MaxTokens   int

	// Per-turn config plumbed into Service.Config.
	SystemPrompt       string
	WindowTurns        int
	MaxConcurrent      int
	DefaultTimeout     time.Duration
	SandboxLimits      sandbox.ResourceLimits
	GateDirectCommands bool

	// Wired-in collaborators.
	Sandbox sandbox.Runner
	FSOps   *fsops.Engine
	History history.Store

	// Approval knobs.
	ApprovalRequiredTools []string
	ApprovalAutoGrant     bool
	ApprovalTimeout       time.Duration

	// GitHub MCP backend (optional). When GitHub is non-nil, dispatcher
	// routes any github_* tool call here; GitHubTools is appended to the
	// per-turn tool catalogue Gemini sees; and every tool name for which
	// IsDestructiveGitHubTool returns true is auto-added to the approval
	// allowlist so the existing gate intercepts mutations.
	GitHub                  GitHubCaller
	GitHubTools             []ToolSpec
	IsDestructiveGitHubTool func(name string) bool

	Logger *slog.Logger
}

// NewService returns a fully wired *Service or nil when Runtime == "none".
// An error is returned for unknown runtimes and for "gemini" when the binary
// was built without the `gemini` build tag.
func NewService(ctx context.Context, c FactoryConfig) (*Service, error) {
	var tr Translator
	switch c.Runtime {
	case "", RuntimeNone:
		return nil, nil
	case RuntimeMock:
		tr = defaultMockTranslator()
	case RuntimeGemini:
		built, err := newGeminiTranslator(ctx, c)
		if err != nil {
			return nil, err
		}
		tr = built
	default:
		return nil, fmt.Errorf("middleware: unknown runtime %q", c.Runtime)
	}

	if c.History == nil {
		return nil, fmt.Errorf("middleware: history store is required")
	}

	approver := NewPolicyApprover(c.ApprovalRequiredTools, c.ApprovalAutoGrant, c.ApprovalTimeout)
	dispatcher := NewCompositeDispatcher(c.Sandbox, c.FSOps)
	tools := DefaultTools()

	if c.GitHub != nil {
		dispatcher.GitHub = c.GitHub
		tools = append(tools, c.GitHubTools...)
		if c.IsDestructiveGitHubTool != nil {
			extra := make([]string, 0, len(c.GitHubTools))
			for _, t := range c.GitHubTools {
				if c.IsDestructiveGitHubTool(t.Name) {
					extra = append(extra, t.Name)
				}
			}
			approver.AddRequired(extra...)
		}
	}

	return &Service{
		Translator: tr,
		Dispatcher: dispatcher,
		Approver:   approver,
		History:    c.History,
		Tools:      tools,
		FSOps:      c.FSOps,
		Config: RuntimeConfig{
			SystemPrompt:       c.SystemPrompt,
			WindowTurns:        c.WindowTurns,
			MaxConcurrent:      c.MaxConcurrent,
			DefaultTimeout:     c.DefaultTimeout,
			SandboxLimits:      c.SandboxLimits,
			GateDirectCommands: c.GateDirectCommands,
		},
	}, nil
}

// defaultMockTranslator returns a mock that emits one assistant chunk and a
// terminal frame — enough for an end-to-end smoke test to see life on the wire.
func defaultMockTranslator() *MockTranslator {
	return NewMockTranslator([]AssistantEvent{
		{Text: "(mock) hello — try -script with execute_script for a fuller flow"},
		{FinalMessage: &FinalMessage{Text: "", FinishReason: "stop"}},
	})
}
