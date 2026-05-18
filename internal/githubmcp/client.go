//go:build github

// Package githubmcp's real Client embeds the upstream github-mcp-server by
// spawning it as a subprocess and talking MCP over stdio. This decouples
// NomadDev from the upstream library's "unstable Go API" warning — we depend
// only on the MCP protocol (which is versioned) and the binary's CLI flags.
//
// Lifecycle: New() spawns the subprocess, performs the MCP initialize
// handshake, calls tools/list once, and caches the result. Each Call()
// re-uses the long-lived session over the same stdio pipe. Close() shuts
// the subprocess down via the SDK's TerminateDuration path (closes stdin,
// then SIGTERM, then SIGKILL).
package githubmcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mattcheramie/nomaddev/internal/middleware"
	"github.com/mattcheramie/nomaddev/internal/sandbox"
)

// Client is the real GitHub MCP backend. Satisfies Caller and
// middleware.GitHubCaller.
type Client struct {
	logger     *slog.Logger
	cmd        *exec.Cmd
	session    *mcp.ClientSession
	transport  *mcp.CommandTransport
	tools      []middleware.ToolSpec
	destrSet   map[string]struct{}
	stderrDone chan struct{}

	// callMu serializes CallTool against the single stdio session. The MCP
	// SDK is JSON-RPC-multiplexed and handles concurrency internally, but
	// the GitHub server's handlers can be slow; a serial gate avoids
	// surprising the upstream with simultaneous mutations.
	callMu sync.Mutex
}

// New spawns the github-mcp-server subprocess, completes the MCP initialize
// handshake, fetches the tool catalogue, and returns a ready-to-use Caller.
func New(ctx context.Context, opts Options) (Caller, error) {
	if opts.Token == nil {
		return nil, errors.New("githubmcp: Options.Token is required")
	}
	token, err := opts.Token.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("githubmcp: token: %w", err)
	}

	bin, err := resolveBinary(opts.BinaryPath)
	if err != nil {
		return nil, err
	}

	args := buildArgs(opts)
	env := buildEnv(token, opts)

	logger := slog.Default()

	// The MCP CommandTransport calls cmd.Start; we hand it a prepared cmd
	// with env + args set and stderr piped to our logger.
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("githubmcp: stderr pipe: %w", err)
	}

	transport := &mcp.CommandTransport{Command: cmd}
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "nomaddev-orchestrator",
		Version: "dev",
	}, nil)

	c := &Client{
		logger:     logger,
		cmd:        cmd,
		transport:  transport,
		stderrDone: make(chan struct{}),
	}

	go c.pipeStderr(stderr)

	startCtx := ctx
	if opts.StartTimeout > 0 {
		var cancel context.CancelFunc
		startCtx, cancel = context.WithTimeout(ctx, opts.StartTimeout)
		defer cancel()
	}

	session, err := client.Connect(startCtx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("githubmcp: connect: %w", err)
	}
	c.session = session

	list, err := session.ListTools(startCtx, nil)
	if err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("githubmcp: list tools: %w", err)
	}

	c.tools, c.destrSet = buildToolList(list.Tools)
	return c, nil
}

// resolveBinary picks the github-mcp-server binary path: explicit option →
// NOMADDEV_GITHUB_MCP_BIN env → "github-mcp-server" on PATH.
func resolveBinary(explicit string) (string, error) {
	candidates := []string{explicit, os.Getenv("NOMADDEV_GITHUB_MCP_BIN"), "github-mcp-server"}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		path, err := exec.LookPath(c)
		if err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("githubmcp: github-mcp-server binary not found (set NOMADDEV_GITHUB_MCP_BIN or install on PATH)")
}

// buildArgs assembles the subprocess CLI args from Options. The upstream
// binary's stdio subcommand reads tokens from env, so the only positional
// arg is "stdio"; everything else is flags.
func buildArgs(opts Options) []string {
	args := []string{"stdio"}
	if len(opts.Toolsets) > 0 && !(len(opts.Toolsets) == 1 && opts.Toolsets[0] == "all") {
		args = append(args, "--toolsets", strings.Join(opts.Toolsets, ","))
	}
	if opts.ReadOnly {
		args = append(args, "--read-only")
	}
	if opts.LockdownMode {
		args = append(args, "--lockdown-mode")
	}
	if opts.Host != "" {
		args = append(args, "--gh-host", opts.Host)
	}
	return args
}

// buildEnv copies the parent env and overrides the GitHub credential. We
// pass through everything else so the subprocess inherits PATH / locale /
// proxy settings; the credential is set last to win any prior value.
func buildEnv(token string, opts Options) []string {
	env := append([]string{}, os.Environ()...)
	env = append(env, "GITHUB_PERSONAL_ACCESS_TOKEN="+token)
	if opts.Host != "" {
		env = append(env, "GITHUB_HOST="+opts.Host)
	}
	return env
}

// pipeStderr forwards the subprocess's stderr to our slog at Info, prefixed
// "github-mcp-server: ". The upstream emits structured logs there; we want
// them threaded into the orchestrator's log stream for one-pane debugging.
func (c *Client) pipeStderr(r io.ReadCloser) {
	defer close(c.stderrDone)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 4096), 64*1024)
	for scanner.Scan() {
		c.logger.Info("github-mcp-server", "line", scanner.Text())
	}
}

// buildToolList converts the upstream tool catalogue into our middleware
// spec shape and computes the destructive-tool set. Each tool's destructive
// status is taken from the upstream's ToolAnnotations.DestructiveHint when
// present (the upstream sets these explicitly); we fall back to the
// IsDestructiveTool heuristic so future / undocumented tools default to
// gated rather than ungated.
func buildToolList(tools []*mcp.Tool) ([]middleware.ToolSpec, map[string]struct{}) {
	specs := make([]middleware.ToolSpec, 0, len(tools))
	destr := make(map[string]struct{}, len(tools))
	for _, t := range tools {
		if t == nil {
			continue
		}
		name := PrefixedName(t.Name)
		schema := middleware.Schema{Type: "object"}
		if t.InputSchema != nil {
			if raw, err := json.Marshal(t.InputSchema); err == nil {
				converted, _ := ConvertSchema(raw)
				schema = converted
			}
		}
		specs = append(specs, middleware.ToolSpec{
			Name:        name,
			Description: t.Description,
			Parameters:  schema,
		})

		if isDestructiveTool(t) {
			destr[name] = struct{}{}
		}
	}
	return specs, destr
}

// isDestructiveTool prefers the upstream's explicit DestructiveHint, falling
// back to our verb heuristic when annotations are absent.
func isDestructiveTool(t *mcp.Tool) bool {
	if t.Annotations != nil {
		if t.Annotations.DestructiveHint != nil {
			return *t.Annotations.DestructiveHint
		}
		// ReadOnlyHint == true is an explicit "safe" signal.
		if t.Annotations.ReadOnlyHint {
			return false
		}
	}
	return IsDestructiveTool(t.Name)
}

// ListTools implements Caller. Returns the cached spec list — the catalogue
// is fetched once at New time and the upstream advertises tools/list_changed
// only when the dynamic-toolsets feature is on (we don't enable it).
func (c *Client) ListTools(_ context.Context) ([]middleware.ToolSpec, error) {
	out := make([]middleware.ToolSpec, len(c.tools))
	copy(out, c.tools)
	return out, nil
}

// Call implements Caller. Strips the github_ prefix, calls the upstream
// tool, and returns the JSON result as a single stdout ExecChunk + terminal
// exit chunk so the wsserver layer can reuse its existing emission path.
func (c *Client) Call(ctx context.Context, call middleware.ToolCall, _ middleware.DispatchOptions) (<-chan sandbox.ExecChunk, error) {
	c.callMu.Lock()
	defer c.callMu.Unlock()

	if c.session == nil {
		return nil, fmt.Errorf("%w: github session not open", sandbox.ErrBadRequest)
	}

	bare := UnprefixedName(call.Tool)
	result, err := c.session.CallTool(ctx, &mcp.CallToolParams{
		Name:      bare,
		Arguments: call.Args,
	})
	if err != nil {
		return c.errorChunk(err.Error()), nil
	}

	payload, marshalErr := encodeResult(result)
	if marshalErr != nil {
		return c.errorChunk(marshalErr.Error()), nil
	}

	exitCode := 0
	if result.IsError {
		exitCode = 1
	}
	ch := make(chan sandbox.ExecChunk, 2)
	ch <- sandbox.ExecChunk{Stream: sandbox.StreamStdout, Data: payload}
	ch <- sandbox.ExecChunk{Stream: sandbox.StreamExit, ExitCode: exitCode}
	close(ch)
	return ch, nil
}

// errorChunk wraps a free-text error in the chunk channel shape the wsserver
// emits as a sandbox-internal error (classifyExit's default branch). Used for
// transport-level failures (connection lost, marshal errors) where there's
// no useful tool result.
func (c *Client) errorChunk(msg string) <-chan sandbox.ExecChunk {
	ch := make(chan sandbox.ExecChunk, 2)
	ch <- sandbox.ExecChunk{
		Stream: sandbox.StreamExit,
		Err:    fmt.Errorf("githubmcp: %s", msg),
	}
	close(ch)
	return ch
}

// encodeResult serializes a CallToolResult into the JSON byte payload the
// translator sees as the tool's stdout. We flatten the SDK's Content slice
// into a stable object: {content: [{type, text}, ...], structured?: any,
// is_error: bool} so Gemini gets a predictable shape.
func encodeResult(r *mcp.CallToolResult) ([]byte, error) {
	type contentEntry struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
	}
	out := struct {
		Content    []contentEntry `json:"content"`
		Structured any            `json:"structured,omitempty"`
		IsError    bool           `json:"is_error,omitempty"`
	}{
		Structured: r.StructuredContent,
		IsError:    r.IsError,
	}
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			out.Content = append(out.Content, contentEntry{Type: "text", Text: tc.Text})
			continue
		}
		// Non-text content (image/audio/embedded resource) is JSON-marshalled
		// and re-attached so the model at least sees structure.
		raw, err := json.Marshal(c)
		if err != nil {
			continue
		}
		out.Content = append(out.Content, contentEntry{Type: "raw", Text: string(raw)})
	}
	return json.Marshal(out)
}

// IsDestructive implements Caller.
func (c *Client) IsDestructive(name string) bool {
	_, ok := c.destrSet[name]
	return ok
}

// Close implements Caller. Tears down the MCP session, which closes the
// subprocess stdin; the SDK's pipeRWC.Close then waits for the process to
// exit, escalating to SIGTERM then SIGKILL on the TerminateDuration.
func (c *Client) Close() error {
	var firstErr error
	if c.session != nil {
		if err := c.session.Close(); err != nil {
			firstErr = err
		}
		c.session = nil
	}
	// Drain stderr goroutine. If the process is gone, the pipe Read returns
	// io.EOF and the scanner loop exits.
	select {
	case <-c.stderrDone:
	case <-time.After(2 * time.Second):
	}
	if c.cmd != nil && c.cmd.Process != nil {
		// Belt-and-suspenders: ensure no zombie if Close raced something.
		_ = c.cmd.Process.Release()
	}
	return firstErr
}
