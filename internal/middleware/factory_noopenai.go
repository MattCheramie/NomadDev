//go:build !openai

package middleware

import (
	"context"
	"errors"
)

// newOpenAITranslator is the stub used when the binary is built without the
// `openai` build tag. The orchestrator surfaces this error verbatim at
// startup so the operator sees what's missing. Both the openai and deepseek
// runtimes route through here.
func newOpenAITranslator(_ context.Context, _ FactoryConfig) (Translator, error) {
	return nil, errors.New("middleware: openai/deepseek runtime requested but binary built without -tags openai")
}
