//go:build gemini

package middleware

import "google.golang.org/genai"

// toGeminiTools converts the SDK-agnostic ToolSpec slice into a genai.Tool
// payload suitable for GenerateContentConfig.Tools.
func toGeminiTools(specs []ToolSpec) []*genai.Tool {
	if len(specs) == 0 {
		return nil
	}
	decls := make([]*genai.FunctionDeclaration, 0, len(specs))
	for _, s := range specs {
		decls = append(decls, &genai.FunctionDeclaration{
			Name:        s.Name,
			Description: s.Description,
			Parameters:  toGeminiSchema(s.Parameters),
		})
	}
	return []*genai.Tool{{FunctionDeclarations: decls}}
}

// toGeminiSchema maps the minimal local Schema into the SDK's richer shape.
func toGeminiSchema(s Schema) *genai.Schema {
	out := &genai.Schema{
		Type:        geminiType(s.Type),
		Description: s.Description,
		Required:    append([]string(nil), s.Required...),
	}
	if len(s.Enum) > 0 {
		out.Enum = append([]string(nil), s.Enum...)
	}
	if len(s.Properties) > 0 {
		out.Properties = make(map[string]*genai.Schema, len(s.Properties))
		for k, v := range s.Properties {
			if v == nil {
				continue
			}
			out.Properties[k] = toGeminiSchema(*v)
		}
	}
	if s.Items != nil {
		out.Items = toGeminiSchema(*s.Items)
	}
	if s.Minimum != nil {
		v := *s.Minimum
		out.Minimum = &v
	}
	if s.Maximum != nil {
		v := *s.Maximum
		out.Maximum = &v
	}
	return out
}

func geminiType(t string) genai.Type {
	switch t {
	case "object":
		return genai.TypeObject
	case "string":
		return genai.TypeString
	case "integer":
		return genai.TypeInteger
	case "number":
		return genai.TypeNumber
	case "boolean":
		return genai.TypeBoolean
	case "array":
		return genai.TypeArray
	}
	return genai.TypeObject
}
