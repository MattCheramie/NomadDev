//go:build openai || anthropic

package middleware

// schemaToJSONMap renders the local Schema into a JSON Schema map[string]any.
// Used by the OpenAI and Anthropic translators since both APIs accept tool
// parameters as freeform JSON Schema objects. (Gemini uses its own typed
// SDK shape — see gemini_tools.go.) Gated by the same build tags as those
// translators so the default binary doesn't carry it.
func schemaToJSONMap(s Schema) map[string]any {
	if s.Type == "" && len(s.Properties) == 0 && s.Items == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	out := map[string]any{}
	if s.Type != "" {
		out["type"] = s.Type
	} else {
		out["type"] = "object"
	}
	if s.Description != "" {
		out["description"] = s.Description
	}
	if len(s.Properties) > 0 {
		props := make(map[string]any, len(s.Properties))
		for k, v := range s.Properties {
			if v == nil {
				continue
			}
			props[k] = schemaToJSONMap(*v)
		}
		out["properties"] = props
	}
	if len(s.Required) > 0 {
		out["required"] = append([]string(nil), s.Required...)
	}
	if len(s.Enum) > 0 {
		out["enum"] = append([]string(nil), s.Enum...)
	}
	if s.Items != nil {
		out["items"] = schemaToJSONMap(*s.Items)
	}
	if s.Minimum != nil {
		out["minimum"] = *s.Minimum
	}
	if s.Maximum != nil {
		out["maximum"] = *s.Maximum
	}
	return out
}
