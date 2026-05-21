package middleware

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/mattcheramie/nomaddev/internal/docfetch"
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

	// WorkerPool* configure the dispatch_worker_pool tool. WorkerPoolEnabled
	// gates whether the tool is appended to the catalogue at all; the rest
	// bound a pool's concurrency, task count, and per-sub-dispatcher timeout.
	WorkerPoolEnabled       bool
	WorkerPoolMaxConcurrent int
	WorkerPoolMaxTasks      int
	WorkerPoolTaskTimeout   time.Duration

	// DaemonMonitorEnabled gates the monitor_daemon / stop_daemon /
	// list_daemons tools: when true they are appended to the catalogue and
	// monitor_daemon is added to the approval allowlist. Mirrors
	// config.SandboxConfig.DaemonEnabled (NOMADDEV_DAEMON_MONITOR_ENABLED).
	DaemonMonitorEnabled bool

	// DocFetchAllowedDomains, when non-empty, pins the fetch_external_docs
	// tool to these domains and their subdomains; every other host is
	// refused. Empty permits any public host (the docfetch exfiltration
	// screen still applies). Sourced from NOMADDEV_DOC_FETCH_ALLOWED_DOMAINS.
	DocFetchAllowedDomains []string

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
	// fetch_external_docs is always-on — its 10s timeout and 2 MB cap are
	// fixed in the docfetch package — so the backend is wired unconditionally.
	// DocFetchAllowedDomains, when set, pins it to an operator allowlist.
	dispatcher.Docs = docfetch.New(docfetch.Config{AllowedDomains: c.DocFetchAllowedDomains})
	tools := DefaultTools()

	// dispatch_worker_pool is opt-in: it is only appended to the catalogue
	// when the operator enabled it. It is always approval-gated — the launch
	// is a single human-approved boundary — so it is added to the approver's
	// required set regardless of NOMADDEV_APPROVAL_REQUIRED_TOOLS.
	if c.WorkerPoolEnabled {
		tools = append(tools, WorkerPoolSpec())
		approver.AddRequired(ToolDispatchWorkerPool)
	}

	// monitor_daemon family is opt-in like the worker pool. monitor_daemon
	// runs an arbitrary host command, so it is always approval-gated
	// regardless of NOMADDEV_APPROVAL_REQUIRED_TOOLS; stop_daemon and
	// list_daemons are not gated (the former only kills a process the session
	// already owns, the latter is read-only).
	if c.DaemonMonitorEnabled {
		tools = append(tools, DaemonToolSpecs()...)
		approver.AddRequired(ToolMonitorDaemon)
	}

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

			WorkerPoolEnabled:       c.WorkerPoolEnabled,
			WorkerPoolMaxConcurrent: c.WorkerPoolMaxConcurrent,
			WorkerPoolMaxTasks:      c.WorkerPoolMaxTasks,
			WorkerPoolTaskTimeout:   c.WorkerPoolTaskTimeout,

			DaemonMonitorEnabled: c.DaemonMonitorEnabled,
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
