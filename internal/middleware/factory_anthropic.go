//go:build anthropic

package middleware

import (
	"context"
	"errors"
	"fmt"
)

func newAnthropicTranslator(ctx context.Context, c FactoryConfig) (Translator, error) {
	if c.APIKey == "" {
		return nil, errors.New("middleware: NOMADDEV_ANTHROPIC_API_KEY is required for runtime=anthropic")
	}
	tr, err := NewAnthropicTranslator(ctx, AnthropicOptions{
		APIKey:         c.APIKey,
		Model:          c.Model,
		Temperature:    c.Temperature,
		MaxTokens:      c.MaxTokens,
		MaxRetries:     c.MaxRetries,
		ThinkingBudget: c.AnthropicThinkingBudget,
		Logger:         c.Logger,
	})
	if err != nil {
		return nil, fmt.Errorf("middleware: anthropic init: %w", err)
	}
	return tr, nil
}
