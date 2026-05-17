//go:build gemini

package middleware

import (
	"context"
	"errors"
	"fmt"
)

func newGeminiTranslator(ctx context.Context, c FactoryConfig) (Translator, error) {
	if c.APIKey == "" {
		return nil, errors.New("middleware: NOMADDEV_GEMINI_API_KEY is required for runtime=gemini")
	}
	tr, err := NewGeminiTranslator(ctx, GeminiOptions{
		APIKey:      c.APIKey,
		Model:       c.Model,
		Temperature: c.Temperature,
		MaxTokens:   c.MaxTokens,
		Logger:      c.Logger,
	})
	if err != nil {
		return nil, fmt.Errorf("middleware: gemini init: %w", err)
	}
	return tr, nil
}
