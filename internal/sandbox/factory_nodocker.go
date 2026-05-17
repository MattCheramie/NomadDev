//go:build !docker

package sandbox

import (
	"context"
	"errors"
)

// newDockerRunner is the stub used when the binary is built without the
// `docker` build tag. The orchestrator surfaces this error verbatim at
// startup so the operator immediately sees what's missing.
func newDockerRunner(_ context.Context, _ FactoryConfig) (Runner, error) {
	return nil, errors.New("sandbox: docker runtime requested but binary built without -tags docker")
}
