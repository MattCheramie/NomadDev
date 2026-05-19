//go:build anthropic

package middleware

import "github.com/anthropics/anthropic-sdk-go"

// toAnthropicTools converts the SDK-agnostic ToolSpec slice into the SDK's
// ToolUnionParam slice. We only ever produce custom function tools, so the
// OfTool variant is the only one we populate. Mirrors gemini_tools.go and
// openai_tools.go.
func toAnthropicTools(specs []ToolSpec) []anthropic.ToolUnionParam {
	if len(specs) == 0 {
		return nil
	}
	out := make([]anthropic.ToolUnionParam, 0, len(specs))
	for _, s := range specs {
		tp := &anthropic.ToolParam{
			Name:        s.Name,
			InputSchema: toAnthropicInputSchema(s.Parameters),
		}
		if s.Description != "" {
			tp.Description = anthropic.String(s.Description)
		}
		out = append(out, anthropic.ToolUnionParam{OfTool: tp})
	}
	return out
}

// toAnthropicInputSchema flattens the local Schema into the SDK's
// ToolInputSchemaParam. The Type field is fixed to "object" by the SDK; we
// stuff Properties straight in and surface Required[] as well. Items / Enum
// / Minimum / Maximum on nested schemas are preserved via the shared JSON
// Schema map (see openai_tools.go).
func toAnthropicInputSchema(s Schema) anthropic.ToolInputSchemaParam {
	out := anthropic.ToolInputSchemaParam{}
	if len(s.Properties) > 0 {
		props := make(map[string]any, len(s.Properties))
		for k, v := range s.Properties {
			if v == nil {
				continue
			}
			props[k] = schemaToJSONMap(*v)
		}
		out.Properties = props
	}
	if len(s.Required) > 0 {
		out.Required = append([]string(nil), s.Required...)
	}
	return out
}
