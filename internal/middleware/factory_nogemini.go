//go:build !gemini

package middleware

import (
	"context"
	"errors"
)

// newGeminiTranslator is the stub used when the binary is built without the
// `gemini` build tag. The orchestrator surfaces this error verbatim at
// startup so the operator sees what's missing.
func newGeminiTranslator(_ context.Context, _ FactoryConfig) (Translator, error) {
	return nil, errors.New("middleware: gemini runtime requested but binary built without -tags gemini")
}
