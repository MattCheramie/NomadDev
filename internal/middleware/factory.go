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
	RuntimeNone      = "none"
	RuntimeMock      = "mock"
	RuntimeGemini    = "gemini"
	RuntimeOpenAI    = "openai"
	RuntimeAnthropic = "anthropic"
	RuntimeDeepSeek  = "deepseek"
)

// deepSeekDefaultBaseURL is the public DeepSeek API endpoint, used when the
// operator selects runtime=deepseek without overriding OpenAIBaseURL. The
// DeepSeek API is OpenAI-compatible so it shares the OpenAI translator.
const deepSeekDefaultBaseURL = "https://api.deepseek.com/v1"

// deepSeekDefaultModel is DeepSeek's general-purpose model. Operators can
// override via NOMADDEV_DEEPSEEK_MODEL.
const deepSeekDefaultModel = "deepseek-chat"

// FactoryConfig is the runtime-agnostic configuration for NewService.
type FactoryConfig struct {
	Runtime string

	// Translator backends.
	APIKey      string
	Model       string
	Temperature float64
	MaxTokens   int

	// OpenAIBaseURL is the API base URL for the OpenAI client. Empty means
	// SDK default (api.openai.com). The deepseek runtime auto-fills this
	// with the DeepSeek endpoint if the operator left it empty.
	OpenAIBaseURL string

	// MaxRetries caps the SDK-level 408/409/429/5xx retry loop for the
	// active translator. Zero keeps each SDK at its default (2 for
	// OpenAI/Anthropic; Gemini's is hardcoded at 3 and not overridable).
	// Sourced from NOMADDEV_LLM_MAX_RETRIES in cmd/orchestrator/main.go.
	MaxRetries int

	// AnthropicThinkingBudget enables Anthropic extended thinking when >0.
	// Value is passed verbatim to ThinkingConfigEnabledParam.BudgetTokens.
	// The Anthropic API requires >= 1024 and < MaxTokens; values that
	// violate either bound are rejected by the API at first call rather
	// than here. Ignored on every non-Anthropic runtime.
	AnthropicThinkingBudget int64

	// Per-turn config plumbed into Service.Config.
	SystemPrompt       string
	WindowTurns        int
	MaxConcurrent      int
	DefaultTimeout     time.Duration
	SandboxLimits      sandbox.ResourceLimits
	GateDirectCommands bool
	MaxAutoRetries     int
	// MaxResultBytes is the structured-tool envelope cap shared between
	// search_syntax (sandbox) and the GitHub MCP backend. Sourced from
	// NOMADDEV_GITHUB_MAX_RESULT_BYTES in cmd/orchestrator/main.go.
	MaxResultBytes int

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

// effectiveDefaultModel returns c.Model when set, falling back to the
// per-runtime hard-coded default. Used so RuntimeConfig.Model always carries
// the model the translator will actually invoke even when the operator left
// NOMADDEV_*_MODEL unset.
func effectiveDefaultModel(runtime, configured string) string {
	if configured != "" {
		return configured
	}
	return defaultModelFor(runtime)
}

// defaultModelFor returns the hard-coded default model for one runtime
// identifier, matching the per-translator fallback. Used so RuntimeConfig.Model
// always carries the actually-active value — the wsserver surfaces this in
// HelloPayload.Model so the mobile picker renders the correct initial
// selection even when the operator left NOMADDEV_*_MODEL unset.
func defaultModelFor(runtime string) string {
	switch runtime {
	case RuntimeOpenAI:
		return "gpt-4o-mini"
	case RuntimeAnthropic:
		return "claude-sonnet-4-5"
	case RuntimeGemini:
		return "gemini-2.0-flash"
	case RuntimeDeepSeek:
		return deepSeekDefaultModel
	}
	return ""
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
	case RuntimeOpenAI:
		built, err := newOpenAITranslator(ctx, c)
		if err != nil {
			return nil, err
		}
		tr = built
	case RuntimeDeepSeek:
		// DeepSeek's API is OpenAI-compatible; the only differences are
		// the base URL and the default model. Defaults applied here so the
		// shared OpenAI translator stays runtime-agnostic.
		if c.OpenAIBaseURL == "" {
			c.OpenAIBaseURL = deepSeekDefaultBaseURL
		}
		if c.Model == "" {
			c.Model = deepSeekDefaultModel
		}
		built, err := newOpenAITranslator(ctx, c)
		if err != nil {
			return nil, err
		}
		tr = built
	case RuntimeAnthropic:
		built, err := newAnthropicTranslator(ctx, c)
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
	pins := history.NewReferenceBuffer()
	dispatcher.Pins = pins
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
		Translator:              tr,
		Dispatcher:              dispatcher,
		Approver:                approver,
		History:                 c.History,
		Tools:                   tools,
		FSOps:                   c.FSOps,
		Pins:                    pins,
		IsDestructiveGitHubTool: c.IsDestructiveGitHubTool,
		Config: RuntimeConfig{
			SystemPrompt:       c.SystemPrompt,
			WindowTurns:        c.WindowTurns,
			MaxConcurrent:      c.MaxConcurrent,
			DefaultTimeout:     c.DefaultTimeout,
			SandboxLimits:      c.SandboxLimits,
			Provider:           c.Runtime,
			Model:              effectiveDefaultModel(c.Runtime, c.Model),
			GateDirectCommands: c.GateDirectCommands,
			MaxAutoRetries:     c.MaxAutoRetries,
			MaxResultBytes:     c.MaxResultBytes,
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
