//go:build openai

package middleware

import (
	"context"
	"errors"
	"fmt"
)

func newOpenAITranslator(ctx context.Context, c FactoryConfig) (Translator, error) {
	if c.APIKey == "" {
		return nil, errors.New("middleware: API key is required for openai-family runtimes (openai/deepseek)")
	}
	tr, err := NewOpenAITranslator(ctx, OpenAIOptions{
		APIKey:      c.APIKey,
		BaseURL:     c.OpenAIBaseURL,
		Model:       c.Model,
		Temperature: c.Temperature,
		MaxTokens:   c.MaxTokens,
		Logger:      c.Logger,
	})
	if err != nil {
		return nil, fmt.Errorf("middleware: openai init: %w", err)
	}
	return tr, nil
}
