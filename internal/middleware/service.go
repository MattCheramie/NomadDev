package middleware

import (
	"time"

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
	Config     RuntimeConfig
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
