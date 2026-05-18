package middleware

import (
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
	FSOps  *fsops.Engine
	Config RuntimeConfig
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

// RuntimeConfig is the per-turn knob set the handler reads from Service.
type RuntimeConfig struct {
	SystemPrompt   string
	WindowTurns    int
	MaxConcurrent  int
	DefaultTimeout time.Duration
	SandboxLimits  sandbox.ResourceLimits
	// GateDirectCommands wires the approval state machine into the legacy
	// direct command.request path (Phase 3) as well as the new
	// middleware-driven path. Default true; the orchestrator sets it from
	// NOMADDEV_APPROVAL_GATE_DIRECT_COMMANDS.
	GateDirectCommands bool
}
