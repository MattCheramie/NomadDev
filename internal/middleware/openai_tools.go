//go:build openai

package middleware

import (
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/shared"
)

// toOpenAITools converts the SDK-agnostic ToolSpec slice into the SDK's
// ChatCompletionToolParam slice. Mirrors gemini_tools.go.
func toOpenAITools(specs []ToolSpec) []openai.ChatCompletionToolParam {
	if len(specs) == 0 {
		return nil
	}
	out := make([]openai.ChatCompletionToolParam, 0, len(specs))
	for _, s := range specs {
		def := shared.FunctionDefinitionParam{
			Name:       s.Name,
			Parameters: toOpenAISchema(s.Parameters),
		}
		if s.Description != "" {
			def.Description = openai.String(s.Description)
		}
		out = append(out, openai.ChatCompletionToolParam{Function: def})
	}
	return out
}

// toOpenAISchema maps the minimal local Schema into JSON Schema shaped as
// map[string]any, which is what FunctionParameters expects. The recursive
// converter (schemaToJSONMap) lives in schema_jsonmap.go and is shared with
// the Anthropic translator.
func toOpenAISchema(s Schema) shared.FunctionParameters {
	return shared.FunctionParameters(schemaToJSONMap(s))
}
