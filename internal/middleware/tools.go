package middleware

import (
	"errors"
	"fmt"
	"strings"
)

// Tool name constants. The middleware speaks these; the sandbox runner
// recognizes ToolExecuteScript; fsops recognizes the remaining three.
const (
	ToolExecuteScript = "execute_script"
	ToolReadFile      = "read_file"
	ToolListDir       = "list_dir"
	ToolWritePatch    = "write_patch"
)

// ErrToolValidation is returned by Validate when the args don't satisfy a
// tool's schema.
var ErrToolValidation = errors.New("middleware: tool args invalid")

// ToolSpec is the SDK-agnostic representation of one tool the model may call.
// The Gemini-tagged build converts these into *genai.FunctionDeclaration in
// gemini_tools.go.
type ToolSpec struct {
	Name        string
	Description string
	Parameters  Schema
}

// Schema is a minimal subset of JSON Schema sufficient for Gemini's
// FunctionDeclaration parameters. Type is one of object/string/integer/number/boolean.
type Schema struct {
	Type        string             `json:"type"`
	Description string             `json:"description,omitempty"`
	Properties  map[string]*Schema `json:"properties,omitempty"`
	Required    []string           `json:"required,omitempty"`
	Enum        []string           `json:"enum,omitempty"`
	Items       *Schema            `json:"items,omitempty"`
	Minimum     *float64           `json:"minimum,omitempty"`
	Maximum     *float64           `json:"maximum,omitempty"`
}

// DefaultTools returns the four-tool catalogue Phase 4 ships.
func DefaultTools() []ToolSpec {
	return []ToolSpec{
		{
			Name: ToolExecuteScript,
			Description: "Run a shell script inside the ephemeral sandbox container. " +
				"Returns stdout, stderr, and the script's exit code. " +
				"Requires human approval before running.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]*Schema{
					"shell":  {Type: "string", Enum: []string{"bash", "sh"}, Description: "shell to run the script (default bash)"},
					"script": {Type: "string", Description: "the shell script to execute"},
				},
				Required: []string{"script"},
			},
		},
		{
			Name: ToolReadFile,
			Description: "Read the contents of a UTF-8 text file inside the orchestrator's workspace. " +
				"Paths are relative to the workspace root; '..' is not allowed.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]*Schema{
					"path":      {Type: "string", Description: "file path relative to the workspace root"},
					"max_bytes": {Type: "integer", Description: "byte cap on the response (default 256 KiB, hard cap 4 MiB)"},
				},
				Required: []string{"path"},
			},
		},
		{
			Name: ToolListDir,
			Description: "List the contents of a directory inside the workspace. " +
				"Returns entries with name, type ('file'|'dir'|'symlink'), and size.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]*Schema{
					"path":  {Type: "string", Description: "directory path relative to the workspace root"},
					"depth": {Type: "integer", Description: "recursion depth (default 1, max 4)"},
				},
				Required: []string{"path"},
			},
		},
		{
			Name: ToolWritePatch,
			Description: "Create, overwrite, or append to a UTF-8 text file inside the workspace. " +
				"Requires human approval before running.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]*Schema{
					"path":    {Type: "string", Description: "file path relative to the workspace root"},
					"content": {Type: "string", Description: "UTF-8 text content to write"},
					"mode":    {Type: "string", Enum: []string{"overwrite", "append"}, Description: "default 'overwrite'"},
					"create":  {Type: "boolean", Description: "create intermediate directories if missing"},
				},
				Required: []string{"path", "content"},
			},
		},
	}
}

// GitHubToolPrefix is reserved for tools provided by the GitHub MCP backend.
// Kept here (not in internal/githubmcp) so the middleware package can recognize
// them without importing the backend — avoids a build-tag dependency cycle.
const GitHubToolPrefix = "github_"

// KnownTool reports whether name is one of the four registered tool names or
// a GitHub MCP tool. The GitHub backend's per-tool schemas are validated by
// the upstream server at dispatch time, so this layer only does a prefix check.
func KnownTool(name string) bool {
	switch name {
	case ToolExecuteScript, ToolReadFile, ToolListDir, ToolWritePatch:
		return true
	}
	return strings.HasPrefix(name, GitHubToolPrefix)
}

// Validate performs lightweight per-tool argument validation before the
// dispatch layer is involved. Heavier checks (path safety for fsops, script
// size for the sandbox) remain in those packages. GitHub MCP tools delegate
// arg validation to the upstream server, so Validate returns nil for any
// github_* name — bad args surface as a tool-result error after dispatch.
func Validate(tool string, args map[string]any) error {
	switch tool {
	case ToolExecuteScript:
		return validateExecuteScript(args)
	case ToolReadFile:
		return validateReadFile(args)
	case ToolListDir:
		return validateListDir(args)
	case ToolWritePatch:
		return validateWritePatch(args)
	}
	if strings.HasPrefix(tool, GitHubToolPrefix) {
		return nil
	}
	return fmt.Errorf("%w: unknown tool %q", ErrToolValidation, tool)
}

func reqString(args map[string]any, key string) (string, error) {
	v, ok := args[key]
	if !ok {
		return "", fmt.Errorf("%w: missing %q", ErrToolValidation, key)
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", fmt.Errorf("%w: %q must be a non-empty string", ErrToolValidation, key)
	}
	return s, nil
}

func validateExecuteScript(args map[string]any) error {
	script, err := reqString(args, "script")
	if err != nil {
		return err
	}
	if len(script) > 64*1024 {
		return fmt.Errorf("%w: script exceeds 64 KiB", ErrToolValidation)
	}
	if shell, ok := args["shell"].(string); ok {
		if shell != "bash" && shell != "sh" && shell != "" {
			return fmt.Errorf("%w: shell must be 'bash' or 'sh'", ErrToolValidation)
		}
	}
	return nil
}

func validateReadFile(args map[string]any) error {
	if _, err := reqString(args, "path"); err != nil {
		return err
	}
	return nil
}

func validateListDir(args map[string]any) error {
	if _, err := reqString(args, "path"); err != nil {
		return err
	}
	return nil
}

func validateWritePatch(args map[string]any) error {
	if _, err := reqString(args, "path"); err != nil {
		return err
	}
	if _, err := reqString(args, "content"); err != nil {
		return err
	}
	if v, ok := args["mode"]; ok {
		s, _ := v.(string)
		switch strings.ToLower(s) {
		case "", "overwrite", "append":
		default:
			return fmt.Errorf("%w: mode must be 'overwrite' or 'append'", ErrToolValidation)
		}
	}
	return nil
}
