package sandbox

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Runtime selectors recognized by NewRunner.
const (
	RuntimeNone   = "none"
	RuntimeMock   = "mock"
	RuntimeDocker = "docker"
)

// FactoryConfig is the runtime-agnostic configuration for NewRunner.
type FactoryConfig struct {
	Runtime        string
	Image          string
	WorkspaceDir   string
	DefaultTimeout time.Duration
	Limits         ResourceLimits
	ReadonlyRoot   bool
	Network        string
	PreferRunsc    bool
	Logger         *slog.Logger
}

// NewRunner returns a Runner for the requested runtime or nil if the runtime
// is "none" (the orchestrator interprets a nil Runner as "command.request
// returns not_implemented"). Returns an error for unknown runtimes and for
// "docker" when the binary was built without the `docker` build tag.
func NewRunner(ctx context.Context, c FactoryConfig) (Runner, error) {
	switch c.Runtime {
	case "", RuntimeNone:
		return nil, nil
	case RuntimeMock:
		// A useful default script for ad-hoc smoke tests: one line of stdout
		// then a clean exit. Tests construct their own MockRunner directly
		// with the script they need.
		return NewMockRunner(MockScript("hello from mock sandbox\n", "", 0)...), nil
	case RuntimeDocker:
		return newDockerRunner(ctx, c)
	default:
		return nil, fmt.Errorf("sandbox: unknown runtime %q", c.Runtime)
	}
}
