//go:build !anthropic

package middleware

import (
	"context"
	"errors"
)

// newAnthropicTranslator is the stub used when the binary is built without
// the `anthropic` build tag. The orchestrator surfaces this error verbatim
// at startup so the operator sees what's missing.
func newAnthropicTranslator(_ context.Context, _ FactoryConfig) (Translator, error) {
	return nil, errors.New("middleware: anthropic runtime requested but binary built without -tags anthropic")
}
