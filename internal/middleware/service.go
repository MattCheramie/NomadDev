package middleware

import (
	"strings"
	"time"

	"github.com/mattcheramie/nomaddev/internal/fsops"
	"github.com/mattcheramie/nomaddev/internal/history"
	"github.com/mattcheramie/nomaddev/internal/sandbox"
)

// Service bundles the four sub-components the orchestrator's middleware
// handler needs. A nil Service signals "no NLP middleware configured" and
// the handler short-circuits to error{not_implemented} for user.intent.
type Service struct {
	Translator Translator
	Dispatcher ToolDispatcher
	Approver   Approver
	History    history.Store
	// Tools is the per-turn catalogue exposed to the translator. Built by
	// NewService as DefaultTools() plus any GitHub MCP tools the factory was
	// handed; the wsserver layer assigns this directly into TurnInput.Tools.
	Tools []ToolSpec
	// FSOps is the same engine the CompositeDispatcher uses; exposed here so
	// the wsserver approval pipeline can call PreviewApplyCodePatch before it
	// builds the tool.approval.request envelope. Optional; nil when the
	// orchestrator is wired without an fsops backend.
	FSOps *fsops.Engine
	// IsDestructiveGitHubTool, when non-nil, classifies a github_* tool as
	// mutating. Wired by NewService from FactoryConfig so the middleware
	// package can stay free of the githubmcp build tag. Used by audit mode
	// to filter the per-turn catalogue.
	IsDestructiveGitHubTool func(name string) bool
	Config                  RuntimeConfig
}

// AvailableTools returns the tool catalogue Service was wired with, with a
// safe fallback to DefaultTools() so older call sites that constructed a
// Service literal without setting Tools keep working.
func (s *Service) AvailableTools() []ToolSpec {
	if s == nil {
		return nil
	}
	if len(s.Tools) == 0 {
		return DefaultTools()
	}
	return s.Tools
}

// IsMutatingTool reports whether the named tool mutates host or remote state.
// Used by audit mode to strip the tool from the per-turn catalogue and to
// refuse dispatch defense-in-depth.
func (s *Service) IsMutatingTool(name string) bool {
	if IsMutatingBaseTool(name) {
		return true
	}
	if strings.HasPrefix(name, GitHubToolPrefix) && s != nil && s.IsDestructiveGitHubTool != nil {
		return s.IsDestructiveGitHubTool(name)
	}
	return false
}

// AvailableToolsFor returns the tool catalogue filtered for the given per-turn
// mode. ModeAudit strips every mutating tool; other modes return the
// unfiltered catalogue.
func (s *Service) AvailableToolsFor(mode string) []ToolSpec {
	all := s.AvailableTools()
	if mode != ModeAudit {
		return all
	}
	out := make([]ToolSpec, 0, len(all))
	for _, t := range all {
		if !s.IsMutatingTool(t.Name) {
			out = append(out, t)
		}
	}
	return out
}

// RuntimeConfig is the per-turn knob set the handler reads from Service.
type RuntimeConfig struct {
	SystemPrompt   string
	WindowTurns    int
	MaxConcurrent  int
	DefaultTimeout time.Duration
	SandboxLimits  sandbox.ResourceLimits
	// Provider is the active translator backend name ("mock", "gemini",
	// "openai", "anthropic", "deepseek"). Used as a Prometheus label on
	// token + cost counters so dashboards can break down spend by backend.
	// Populated from FactoryConfig.Runtime.
	Provider string
	// Model is the active model identifier (e.g. "gpt-4o-mini",
	// "claude-sonnet-4-5"). Pairs with Provider for the pricing lookup.
	// Populated from FactoryConfig.Model — for the deepseek runtime, the
	// factory pre-fills the default before the service is constructed.
	Model string
	// GateDirectCommands wires the approval state machine into the legacy
	// direct command.request path (Phase 3) as well as the new
	// middleware-driven path. Default true; the orchestrator sets it from
	// NOMADDEV_APPROVAL_GATE_DIRECT_COMMANDS.
	GateDirectCommands bool
	// MaxAutoRetries caps consecutive failed tool-call dispatches inside
	// one chain before the orchestration loop escalates the failure to
	// the Mobile Control Hub. Propagated from
	// config.MiddlewareConfig.MaxAutoRetries (NOMADDEV_MAX_AUTORETRIES).
	// 0 disables the recovery loop; the first retryable failure escalates.
	MaxAutoRetries int
	// MaxResultBytes is the structured-tool envelope cap forwarded to
	// the sandbox runner via DispatchOptions for search_syntax calls.
	// Sourced from NOMADDEV_GITHUB_MAX_RESULT_BYTES; 0 = unlimited.
	MaxResultBytes int
}
