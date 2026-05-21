package middleware

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Tool name constants. The middleware speaks these; the sandbox runner
// recognizes ToolExecuteScript and ToolSearchSyntax; fsops recognizes the
// four read/write tools; ToolPinFile and ToolUnpinFile are served by the
// dispatcher's persistent-reference-buffer path.
const (
	ToolExecuteScript  = "execute_script"
	ToolReadFile       = "read_file"
	ToolListDir        = "list_dir"
	ToolWritePatch     = "write_patch"
	ToolApplyCodePatch = "apply_code_patch"
	ToolSearchSyntax   = "search_syntax"
	ToolPinFile        = "pin_file"
	ToolUnpinFile      = "unpin_file"
	// fetch_external_docs retrieves an external http(s) documentation page,
	// strips it to markdown, and returns the text. Served in-process by the
	// dispatcher's docfetch backend — read-only, so it needs no approval and
	// stays available in audit mode.
	ToolFetchExternalDocs = "fetch_external_docs"
	// Daemon tools — opt-in (NOMADDEV_DAEMON_MONITOR_ENABLED). monitor_daemon
	// runs a detached host process; stop_daemon / list_daemons manage them.
	// Served by the wsserver daemon path, not the CompositeDispatcher.
	ToolMonitorDaemon = "monitor_daemon"
	ToolStopDaemon    = "stop_daemon"
	ToolListDaemons   = "list_daemons"
)

// ErrToolValidation is returned by Validate when the args don't satisfy a
// tool's schema.
var ErrToolValidation = errors.New("middleware: tool args invalid")

// Turn-mode values mirrored from event.UserIntentMode*. Kept here so the
// middleware package can act on the mode without importing the event package
// (which would create a cycle for some downstream callers).
const (
	ModeNormal = ""
	ModeAudit  = "audit"
)

// IsMutatingBaseTool reports whether one of the base tools mutates host
// state. GitHub MCP tools are classified separately via the caller-supplied
// predicate plumbed through Service.IsDestructiveGitHubTool — execute_script
// is counted as mutating because it can run arbitrary shell, including writes.
// pin_file / unpin_file are not mutating: they only touch the in-memory
// reference buffer, so they stay available in audit mode.
func IsMutatingBaseTool(name string) bool {
	switch name {
	case ToolExecuteScript, ToolWritePatch, ToolApplyCodePatch, ToolDispatchWorkerPool,
		ToolMonitorDaemon, ToolStopDaemon:
		return true
	}
	return false
}

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

// DefaultTools returns the base tool catalogue exposed to the model.
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
		{
			Name: ToolApplyCodePatch,
			Description: "Apply a single search/replace edit to a UTF-8 text file inside the workspace. " +
				"search_string must occur exactly once in the target file. " +
				"Requires human approval; a unified-diff preview is rendered in the ApprovalSheet before the write. " +
				"When verify_command is set, the orchestrator runs that shell command in the ephemeral " +
				"sandbox immediately after the write; a non-zero exit rolls the file back to its " +
				"pre-edit contents and surfaces the verify command's stderr as the tool result, which " +
				"feeds the automated-recovery loop the same way any retryable failure does.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]*Schema{
					"file_path":      {Type: "string", Description: "file path relative to the workspace root"},
					"search_string":  {Type: "string", Description: "anchor that must occur exactly once in the file"},
					"replace_string": {Type: "string", Description: "replacement text; may be empty for pure deletion"},
					"verify_command": {Type: "string", Description: "optional shell command run in the sandbox after the patch lands; non-zero exit triggers automatic rollback (e.g. 'go build ./...' or 'golangci-lint run')"},
				},
				Required: []string{"file_path", "search_string", "replace_string"},
			},
		},
		{
			Name: ToolSearchSyntax,
			Description: "Run a structural ast-grep query against the workspace. " +
				"Use AST patterns (e.g. 'fn $F($_: context.Context)') instead of regex; " +
				"meta-variables: $VAR (single node), $$$VAR (multi-node), $_ (anonymous). " +
				"Returns matches with file, line, column, and a code snippet. " +
				"Read-only; does not require approval.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]*Schema{
					"pattern":     {Type: "string", Description: "ast-grep pattern"},
					"lang":        {Type: "string", Description: "language hint (go, ts, tsx, js, py, rs, java, …). Inferred from extensions when omitted."},
					"path":        {Type: "string", Description: "subdirectory to search relative to the workspace root; defaults to the whole tree"},
					"max_matches": {Type: "integer", Description: "soft cap on matches returned (default 100, max 1000)"},
					"globs":       {Type: "array", Items: &Schema{Type: "string"}, Description: "optional glob filters, forwarded as --globs"},
				},
				Required: []string{"pattern"},
			},
		},
		{
			Name: ToolPinFile,
			Description: "Pin the current contents of a UTF-8 text file into the persistent " +
				"reference buffer. Pinned files are injected at the top of the system prompt on " +
				"every turn and survive history compaction, so use this to keep critical " +
				"architectural files in context during long, multi-step tasks. Paths are relative " +
				"to the workspace root; '..' is not allowed. Re-pinning the same path refreshes " +
				"its stored contents. A file larger than the read cap (256 KiB) is pinned " +
				"truncated. Read-only; does not require approval. Release a pin with unpin_file.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]*Schema{
					"path": {Type: "string", Description: "file path relative to the workspace root"},
				},
				Required: []string{"path"},
			},
		},
		{
			Name: ToolUnpinFile,
			Description: "Remove a file from the persistent reference buffer once it is no longer " +
				"needed for the current task, freeing the memory and prompt space it occupied. " +
				"Paths are relative to the workspace root. Unpinning a file that is not pinned is " +
				"a no-op, not an error. Read-only; does not require approval.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]*Schema{
					"path": {Type: "string", Description: "file path relative to the workspace root"},
				},
				Required: []string{"path"},
			},
		},
		{
			Name: ToolFetchExternalDocs,
			Description: "Fetch a documentation page from an external http(s) URL and return it " +
				"as plain markdown text. The orchestrator issues a single GET request, strips " +
				"all HTML, CSS, scripts and navigation chrome, and returns just the readable " +
				"text — use this to re-check an external API's current schema when a script is " +
				"failing against it. Requests to private, loopback or link-local addresses are " +
				"refused; the response is capped at 2 MB and the request times out after 10 " +
				"seconds. Read-only; does not require approval.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]*Schema{
					"url": {Type: "string", Description: "absolute http(s) URL of the documentation page to fetch"},
				},
				Required: []string{"url"},
			},
		},
	}
}

// DaemonToolSpecs returns the ToolSpecs for monitor_daemon / stop_daemon /
// list_daemons. Kept out of DefaultTools() so the factory appends them only
// when the feature is enabled — the same opt-in shape as WorkerPoolSpec.
func DaemonToolSpecs() []ToolSpec {
	return []ToolSpec{
		{
			Name: ToolMonitorDaemon,
			Description: "Start a long-running daemon process — a dev server, file " +
				"watcher, or log tailer — as a detached process on the orchestrator host. " +
				"The call returns immediately; the daemon keeps running in the background " +
				"and its stdout/stderr stream back afterward as system.log_event envelopes. " +
				"Each daemon gets an id: use stop_daemon to terminate it and list_daemons " +
				"to see what is running. Requires human approval.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]*Schema{
					"command":     {Type: "string", Description: "the shell command to run as a background daemon"},
					"working_dir": {Type: "string", Description: "optional working directory for the daemon process"},
				},
				Required: []string{"command"},
			},
		},
		{
			Name: ToolStopDaemon,
			Description: "Stop a daemon previously started with monitor_daemon, " +
				"terminating its whole process group.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]*Schema{
					"daemon_id": {Type: "string", Description: "the daemon id returned by monitor_daemon"},
				},
				Required: []string{"daemon_id"},
			},
		},
		{
			Name: ToolListDaemons,
			Description: "List the daemons currently running for this session, each with " +
				"its id, command, and uptime. Read-only; does not require approval.",
			Parameters: Schema{
				Type:       "object",
				Properties: map[string]*Schema{},
			},
		},
	}
}

// GitHubToolPrefix is reserved for tools provided by the GitHub MCP backend.
// Kept here (not in internal/githubmcp) so the middleware package can recognize
// them without importing the backend — avoids a build-tag dependency cycle.
const GitHubToolPrefix = "github_"

// KnownTool reports whether name is one of the registered base tool names or
// a GitHub MCP tool. The GitHub backend's per-tool schemas are validated by
// the upstream server at dispatch time, so this layer only does a prefix check.
func KnownTool(name string) bool {
	switch name {
	case ToolExecuteScript, ToolReadFile, ToolListDir, ToolWritePatch, ToolApplyCodePatch,
		ToolSearchSyntax, ToolPinFile, ToolUnpinFile, ToolFetchExternalDocs, ToolDispatchWorkerPool,
		ToolMonitorDaemon, ToolStopDaemon, ToolListDaemons:
		return true
	}
	return strings.HasPrefix(name, GitHubToolPrefix)
}

// Validate performs lightweight per-tool argument validation before the
// dispatch layer is involved. Heavier checks (path safety for fsops, script
// size for the sandbox, sg-argv shape for search_syntax) remain in those
// packages. GitHub MCP tools delegate
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
	case ToolApplyCodePatch:
		return validateApplyCodePatch(args)
	case ToolSearchSyntax:
		return validateSearchSyntax(args)
	case ToolPinFile:
		return validatePinFile(args)
	case ToolUnpinFile:
		return validateUnpinFile(args)
	case ToolFetchExternalDocs:
		return validateFetchExternalDocs(args)
	case ToolDispatchWorkerPool:
		return validateDispatchWorkerPool(args)
	case ToolMonitorDaemon:
		return validateMonitorDaemon(args)
	case ToolStopDaemon:
		return validateStopDaemon(args)
	case ToolListDaemons:
		return nil
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

func validatePinFile(args map[string]any) error {
	if _, err := reqString(args, "path"); err != nil {
		return err
	}
	return nil
}

func validateUnpinFile(args map[string]any) error {
	if _, err := reqString(args, "path"); err != nil {
		return err
	}
	return nil
}

// validateFetchExternalDocs checks the url arg up-front: it must be a
// non-empty, parseable absolute http(s) URL of a sane length. The deeper SSRF
// screening (rejecting private/loopback addresses) needs DNS resolution and so
// runs in the docfetch backend at dispatch time, not here.
func validateFetchExternalDocs(args map[string]any) error {
	rawURL, err := reqString(args, "url")
	if err != nil {
		return err
	}
	if len(rawURL) > 2048 {
		return fmt.Errorf("%w: url exceeds 2048 bytes", ErrToolValidation)
	}
	u, perr := url.Parse(strings.TrimSpace(rawURL))
	if perr != nil {
		return fmt.Errorf("%w: url is not parseable: %v", ErrToolValidation, perr)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("%w: url scheme must be http or https", ErrToolValidation)
	}
	if u.Host == "" {
		return fmt.Errorf("%w: url must include a host", ErrToolValidation)
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

func validateSearchSyntax(args map[string]any) error {
	pattern, err := reqString(args, "pattern")
	if err != nil {
		return err
	}
	if len(pattern) > 8*1024 {
		return fmt.Errorf("%w: pattern exceeds 8 KiB", ErrToolValidation)
	}
	if v, ok := args["path"]; ok {
		s, sok := v.(string)
		if !sok {
			return fmt.Errorf("%w: %q must be a string", ErrToolValidation, "path")
		}
		if strings.HasPrefix(s, "/") {
			return fmt.Errorf("%w: %q must be relative to the workspace root", ErrToolValidation, "path")
		}
		// Reject any ".." segment up-front so a bad pattern fails before
		// dispatch. The sandbox-side builder repeats this check
		// defense-in-depth.
		if s == ".." || strings.HasPrefix(s, "../") || strings.Contains(s, "/../") {
			return fmt.Errorf("%w: %q must not contain '..'", ErrToolValidation, "path")
		}
	}
	if v, ok := args["lang"]; ok {
		s, sok := v.(string)
		if !sok {
			return fmt.Errorf("%w: %q must be a string", ErrToolValidation, "lang")
		}
		if len(s) > 16 {
			return fmt.Errorf("%w: lang exceeds 16 chars", ErrToolValidation)
		}
		for _, r := range s {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
				return fmt.Errorf("%w: lang must be alphabetic", ErrToolValidation)
			}
		}
	}
	if v, ok := args["max_matches"]; ok {
		switch n := v.(type) {
		case int:
			if n <= 0 || n > 1000 {
				return fmt.Errorf("%w: max_matches must be 1..1000", ErrToolValidation)
			}
		case int64:
			if n <= 0 || n > 1000 {
				return fmt.Errorf("%w: max_matches must be 1..1000", ErrToolValidation)
			}
		case float64:
			if n <= 0 || n > 1000 {
				return fmt.Errorf("%w: max_matches must be 1..1000", ErrToolValidation)
			}
		default:
			return fmt.Errorf("%w: max_matches must be an integer", ErrToolValidation)
		}
	}
	if v, ok := args["globs"]; ok {
		switch list := v.(type) {
		case []any:
			for _, g := range list {
				s, sok := g.(string)
				if !sok {
					return fmt.Errorf("%w: globs must be a list of strings", ErrToolValidation)
				}
				if len(s) > 256 {
					return fmt.Errorf("%w: a glob exceeds 256 bytes", ErrToolValidation)
				}
			}
		case []string:
			for _, s := range list {
				if len(s) > 256 {
					return fmt.Errorf("%w: a glob exceeds 256 bytes", ErrToolValidation)
				}
			}
		default:
			return fmt.Errorf("%w: globs must be a list of strings", ErrToolValidation)
		}
	}
	return nil
}

func validateApplyCodePatch(args map[string]any) error {
	if _, err := reqString(args, "file_path"); err != nil {
		return err
	}
	if _, err := reqString(args, "search_string"); err != nil {
		return err
	}
	// replace_string may be the empty string (pure deletion) but must be present
	// and of string type.
	v, ok := args["replace_string"]
	if !ok {
		return fmt.Errorf("%w: missing %q", ErrToolValidation, "replace_string")
	}
	if _, ok := v.(string); !ok {
		return fmt.Errorf("%w: %q must be a string", ErrToolValidation, "replace_string")
	}
	if v, ok := args["verify_command"]; ok {
		s, sok := v.(string)
		if !sok {
			return fmt.Errorf("%w: %q must be a string", ErrToolValidation, "verify_command")
		}
		if len(s) > 8*1024 {
			return fmt.Errorf("%w: verify_command exceeds 8 KiB", ErrToolValidation)
		}
	}
	return nil
}

func validateMonitorDaemon(args map[string]any) error {
	command, err := reqString(args, "command")
	if err != nil {
		return err
	}
	if len(command) > 64*1024 {
		return fmt.Errorf("%w: command exceeds 64 KiB", ErrToolValidation)
	}
	if v, ok := args["working_dir"]; ok {
		if _, sok := v.(string); !sok {
			return fmt.Errorf("%w: %q must be a string", ErrToolValidation, "working_dir")
		}
	}
	return nil
}

func validateStopDaemon(args map[string]any) error {
	if _, err := reqString(args, "daemon_id"); err != nil {
		return err
	}
	return nil
}
