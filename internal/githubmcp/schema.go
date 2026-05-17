package githubmcp

import (
	"encoding/json"
	"strings"

	"github.com/mattcheramie/nomaddev/internal/middleware"
)

// ToolPrefix is prepended to every MCP tool name as it crosses into the
// middleware. The wsserver dispatcher routes calls back via strings.HasPrefix.
const ToolPrefix = "github_"

// rawSchema is the loose JSON-Schema-ish shape the upstream MCP server emits
// per tool. We only decode the subset Gemini cares about; unsupported
// constructs (oneOf, $ref, anyOf) degrade to a free-form string param
// rather than failing the whole tool registration.
type rawSchema struct {
	Type        string                `json:"type,omitempty"`
	Description string                `json:"description,omitempty"`
	Properties  map[string]*rawSchema `json:"properties,omitempty"`
	Required    []string              `json:"required,omitempty"`
	Enum        []json.RawMessage     `json:"enum,omitempty"`
	Items       *rawSchema            `json:"items,omitempty"`
	Minimum     *float64              `json:"minimum,omitempty"`
	Maximum     *float64              `json:"maximum,omitempty"`
}

// ConvertSchema converts an MCP tool's raw JSON Schema bytes into the
// middleware's Schema shape (which Gemini consumes via gemini_tools.go).
// Returns a usable Schema even on partial parses — Gemini is tolerant of
// extra fields and a partial schema is far better than dropping the tool.
func ConvertSchema(raw []byte) (middleware.Schema, error) {
	if len(raw) == 0 {
		// Tools without input schemas (e.g. github_get_me) take no args.
		return middleware.Schema{Type: "object"}, nil
	}
	var r rawSchema
	if err := json.Unmarshal(raw, &r); err != nil {
		// Fall back to a free-form object so the tool still registers.
		return middleware.Schema{
			Type:        "object",
			Description: "schema unavailable (parse failed); see upstream docs",
		}, err
	}
	return convert(&r), nil
}

func convert(r *rawSchema) middleware.Schema {
	if r == nil {
		return middleware.Schema{}
	}
	out := middleware.Schema{
		Type:        normalizeType(r.Type),
		Description: r.Description,
		Required:    append([]string(nil), r.Required...),
		Minimum:     r.Minimum,
		Maximum:     r.Maximum,
	}
	if len(r.Enum) > 0 {
		out.Enum = enumToStrings(r.Enum)
	}
	if len(r.Properties) > 0 {
		out.Properties = make(map[string]*middleware.Schema, len(r.Properties))
		for k, v := range r.Properties {
			if v == nil {
				continue
			}
			s := convert(v)
			out.Properties[k] = &s
		}
	}
	if r.Items != nil {
		s := convert(r.Items)
		out.Items = &s
	}
	return out
}

// normalizeType maps the input to one of the strings the middleware.Schema
// type expects. Empty or unrecognized → "object" so Gemini accepts the
// declaration.
func normalizeType(t string) string {
	switch strings.ToLower(t) {
	case "object", "string", "integer", "number", "boolean", "array":
		return strings.ToLower(t)
	case "":
		// MCP tools sometimes omit the top-level type when properties are
		// present. Default to object so Gemini accepts the declaration.
		return "object"
	}
	return "string"
}

// enumToStrings flattens an enum list into the string slice middleware.Schema
// expects. Non-string enum values are JSON-marshalled so the user sees a
// readable hint (e.g. ints become "1", "2"). Gemini's function-calling
// implementation tolerates this when the actual arg comes back as the right
// JSON type.
func enumToStrings(raw []json.RawMessage) []string {
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s := string(v)
		// Strip enclosing quotes when the raw value is a JSON string.
		if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
			var unquoted string
			if err := json.Unmarshal(v, &unquoted); err == nil {
				out = append(out, unquoted)
				continue
			}
		}
		out = append(out, s)
	}
	return out
}

// PrefixedName returns the MCP tool name as the middleware should see it.
// Idempotent: an already-prefixed name passes through unchanged.
func PrefixedName(mcpName string) string {
	if strings.HasPrefix(mcpName, ToolPrefix) {
		return mcpName
	}
	return ToolPrefix + mcpName
}

// UnprefixedName strips the middleware prefix to recover the upstream MCP
// tool name. Idempotent.
func UnprefixedName(prefixedName string) string {
	return strings.TrimPrefix(prefixedName, ToolPrefix)
}

// IsGitHubTool reports whether a tool name was minted by this package.
func IsGitHubTool(name string) bool {
	return strings.HasPrefix(name, ToolPrefix)
}
