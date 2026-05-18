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
	"math/rand"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/mattcheramie/nomaddev/internal/metrics"
	"github.com/mattcheramie/nomaddev/internal/middleware"
	"github.com/mattcheramie/nomaddev/internal/sandbox"
)

// Client is the real GitHub MCP backend. Satisfies Caller and
// middleware.GitHubCaller.
type Client struct {
	// opts is stashed so respawn() can rebuild the subprocess without
	// re-asking the caller for credentials / toolset config.
	opts   Options
	logger *slog.Logger

	// sessionMu protects the mutable subprocess + session fields below.
	// Held in write mode during spawn/teardown/respawn, read mode during a
	// CallTool. The MCP SDK handles concurrent CallTool itself; sessionMu
	// only serializes against restarts.
	sessionMu sync.RWMutex
	cmd       *exec.Cmd
	session   *mcp.ClientSession

	// tools / destrSet are populated once during the first spawn and treated
	// as immutable thereafter — the upstream catalogue doesn't change at
	// runtime (we disable --dynamic-toolsets), so respawn doesn't re-fetch
	// and Gemini's tool list stays stable across a subprocess restart.
	tools    []middleware.ToolSpec
	destrSet map[string]struct{}

	// stderrDone is closed when the stderr-pipe goroutine for the current
	// subprocess exits. Replaced on each spawn.
	stderrDone chan struct{}

	// lastRestart caps the restart rate. The first restart after a
	// successful spawn happens immediately; subsequent restarts within
	// restartCooldown are refused so a panicking subprocess doesn't trigger
	// an infinite respawn loop. The window resets on a successful Call.
	lastRestart time.Time

	// callMu serializes CallTool against the single stdio session. The MCP
	// SDK is JSON-RPC-multiplexed and handles concurrency internally, but
	// the GitHub server's handlers can be slow; a serial gate avoids
	// surprising the upstream with simultaneous mutations.
	callMu sync.Mutex
}

// restartCooldown is the minimum time between subprocess restart attempts.
// Caps the runaway-respawn worst case to ~12 restarts/minute if the
// subprocess crashes every time it boots.
const restartCooldown = 5 * time.Second

// New spawns the github-mcp-server subprocess, completes the MCP initialize
// handshake, fetches the tool catalogue, and returns a ready-to-use Caller.
// Errors from the first spawn are returned synchronously so the orchestrator
// fails its startup if the operator misconfigured the binary or token.
// Errors from later respawns (after a subprocess crash) surface through Call.
func New(ctx context.Context, opts Options) (Caller, error) {
	if opts.Token == nil {
		return nil, errors.New("githubmcp: Options.Token is required")
	}
	c := &Client{
		opts:   opts,
		logger: slog.Default(),
	}
	if err := c.spawn(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// spawn does the subprocess + MCP session setup. The first call (from New)
// also fetches the tool catalogue; subsequent calls (from respawn) skip
// that because the upstream's tools don't change between restarts.
//
// Acquires sessionMu in write mode. Callers must not hold any other lock.
func (c *Client) spawn(ctx context.Context) error {
	token, err := c.opts.Token.Token(ctx)
	if err != nil {
		return fmt.Errorf("githubmcp: token: %w", err)
	}

	bin, err := resolveBinary(c.opts.BinaryPath)
	if err != nil {
		return err
	}

	args := buildArgs(c.opts)
	env := buildEnv(token, c.opts)

	// exec.CommandContext + the SDK's CommandTransport.Connect handles
	// Start(); we just prepare the cmd with env, args, and a stderr pipe.
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("githubmcp: stderr pipe: %w", err)
	}

	transport := &mcp.CommandTransport{Command: cmd}
	mcpClient := mcp.NewClient(&mcp.Implementation{
		Name:    "nomaddev-orchestrator",
		Version: "dev",
	}, nil)

	stderrDone := make(chan struct{})
	go c.pipeStderr(stderr, stderrDone)

	startCtx := ctx
	if c.opts.StartTimeout > 0 {
		var cancel context.CancelFunc
		startCtx, cancel = context.WithTimeout(ctx, c.opts.StartTimeout)
		defer cancel()
	}

	session, err := mcpClient.Connect(startCtx, transport, nil)
	if err != nil {
		return fmt.Errorf("githubmcp: connect: %w", err)
	}

	// First-spawn-only: fetch the tool catalogue. We assume the upstream
	// doesn't add/remove tools across restarts; --dynamic-toolsets is off.
	freshTools := c.tools == nil
	var tools []middleware.ToolSpec
	var destrSet map[string]struct{}
	if freshTools {
		list, listErr := session.ListTools(startCtx, nil)
		if listErr != nil {
			_ = session.Close()
			return fmt.Errorf("githubmcp: list tools: %w", listErr)
		}
		tools, destrSet = buildToolList(list.Tools)
	}

	c.sessionMu.Lock()
	c.cmd = cmd
	c.session = session
	c.stderrDone = stderrDone
	if freshTools {
		c.tools = tools
		c.destrSet = destrSet
	}
	c.sessionMu.Unlock()
	return nil
}

// teardown closes the current session and waits for the stderr goroutine
// to drain. Safe to call when fields are nil (e.g., after a failed spawn).
// Caller must hold sessionMu in write mode.
func (c *Client) teardown() {
	if c.session != nil {
		_ = c.session.Close()
		c.session = nil
	}
	if c.stderrDone != nil {
		select {
		case <-c.stderrDone:
		case <-time.After(2 * time.Second):
			// Subprocess pipe is stuck; not worth blocking restart on.
		}
		c.stderrDone = nil
	}
	c.cmd = nil
}

// respawn tears down a dead subprocess and starts a fresh one. Cooldown-
// throttled so a flapping upstream binary can't trigger an infinite restart
// loop. Returns an error only when the cooldown was tripped or spawn failed
// outright; caller decides whether to surface that to the model.
func (c *Client) respawn(ctx context.Context) error {
	c.sessionMu.Lock()
	if !c.lastRestart.IsZero() && time.Since(c.lastRestart) < restartCooldown {
		c.sessionMu.Unlock()
		return fmt.Errorf("githubmcp: subprocess restart cooldown active (last attempt %s ago)",
			time.Since(c.lastRestart).Round(time.Millisecond))
	}
	c.lastRestart = time.Now()
	c.teardown()
	c.sessionMu.Unlock()

	c.logger.Warn("github-mcp-server: subprocess died, respawning")
	if err := c.spawn(ctx); err != nil {
		c.logger.Error("github-mcp-server: respawn failed", "err", err)
		return err
	}
	c.logger.Info("github-mcp-server: respawn complete")
	return nil
}

// subprocessDied reports whether the current cmd has exited. Used by Call to
// decide whether a CallTool error is "upstream returned an error" (don't
// restart) or "transport is dead" (restart and retry once).
func (c *Client) subprocessDied() bool {
	c.sessionMu.RLock()
	defer c.sessionMu.RUnlock()
	if c.cmd == nil {
		return true
	}
	if c.cmd.ProcessState != nil {
		return true
	}
	// ProcessState is only set after cmd.Wait returns; the SDK's pipeRWC
	// calls Wait inside its Close, not proactively. A Signal(0) probe tells
	// us liveness without committing to a Wait we can't undo.
	if c.cmd.Process == nil {
		return true
	}
	if err := c.cmd.Process.Signal(syscall.Signal(0)); err != nil {
		return true
	}
	return false
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
// done is closed when the goroutine exits — used by teardown to wait out a
// dying subprocess before declaring restart complete.
func (c *Client) pipeStderr(r io.ReadCloser, done chan struct{}) {
	defer close(done)
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
//
// opts.Timeout, when > 0, derives a child context that caps the upstream
// MCP CallTool round-trip. classifyExit() in wsserver maps the resulting
// context.DeadlineExceeded into event.SandboxErrTimeout so the assistant
// turn sees a graceful timeout instead of hanging on a stuck remote API.
//
// If the upstream subprocess has died (crash, OOM, kill -9), Call detects
// the dead pipe, respawns the binary, and retries the tool call exactly
// once before surfacing the error. Cooldown-throttled in respawn() so a
// flapping upstream can't loop.
func (c *Client) Call(ctx context.Context, call middleware.ToolCall, opts middleware.DispatchOptions) (<-chan sandbox.ExecChunk, error) {
	c.callMu.Lock()
	defer c.callMu.Unlock()

	// Phase 11.3: per-call span. Noop when tracing is disabled.
	// Attributes are intentionally narrow (tool, session) — arg
	// values would dwarf the trace storage and leak secrets.
	tracer := otel.Tracer("nomaddev/githubmcp")
	ctx, span := tracer.Start(ctx, "github.call",
		trace.WithAttributes(
			attribute.String("github.tool", call.Tool),
			attribute.String("github.session_id", opts.SessionID),
		),
	)
	defer span.End()

	c.sessionMu.RLock()
	session := c.session
	c.sessionMu.RUnlock()
	if session == nil {
		return nil, fmt.Errorf("%w: github session not open", sandbox.ErrBadRequest)
	}

	// Enforce the arg-size cap before any context / subprocess work — a
	// misbehaving LLM that emits a 100 MB blob shouldn't OOM the stdio
	// pipe. The check is symmetric with sandbox.ToolExecuteScript's 64 KiB
	// script cap; the larger default here (256 KiB) reflects that GitHub
	// payloads legitimately carry larger bodies (PR descriptions, commit
	// messages with diffs).
	if c.opts.MaxArgBytes > 0 {
		if argBytes, mErr := json.Marshal(call.Args); mErr == nil {
			if len(argBytes) > c.opts.MaxArgBytes {
				return c.errorChunkBadRequest(fmt.Sprintf(
					"arguments exceed cap (%d bytes > %d; raise NOMADDEV_GITHUB_MAX_ARG_BYTES to allow)",
					len(argBytes), c.opts.MaxArgBytes,
				)), nil
			}
		}
	}

	callCtx := ctx
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	bare := UnprefixedName(call.Tool)
	params := &mcp.CallToolParams{Name: bare, Arguments: call.Args}
	result, err := session.CallTool(callCtx, params)
	if err != nil {
		// Context cancellation / deadline must round-trip cleanly so
		// classifyExit maps to SandboxErrTimeout / SandboxErrCanceled.
		// These are NOT subprocess crashes — leave the subprocess alone.
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return c.contextErrorChunk(err), nil
		}

		// Subprocess-died detection: if the cmd has exited, the stdio
		// transport is unrecoverable. Respawn and retry once before
		// surfacing the error to the translator.
		if c.subprocessDied() {
			if rerr := c.respawn(ctx); rerr != nil {
				return c.errorChunk(fmt.Sprintf("upstream subprocess died and respawn failed: %v", rerr)), nil
			}
			c.sessionMu.RLock()
			session = c.session
			c.sessionMu.RUnlock()
			if session == nil {
				return c.errorChunk("upstream subprocess died and respawn left no session"), nil
			}
			// One retry. If this also fails, surface the error to the
			// model — endless retries on a poison call are worse than a
			// graceful turn-level error.
			result, err = session.CallTool(callCtx, params)
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
					return c.contextErrorChunk(err), nil
				}
				return c.errorChunk(err.Error()), nil
			}
		} else {
			return c.errorChunk(err.Error()), nil
		}
	}

	// Phase 8.9: GitHub rate-limit retry. The upstream surfaces a
	// 429 / secondary-rate-limit / abuse-detection failure as a
	// CallToolResult with IsError=true and human-readable text we
	// can pattern-match. Loop with bounded exponential backoff
	// (honoring any Retry-After hint in the message) up to the
	// configured max attempts before surfacing the error.
	if c.opts.RateLimitRetries > 0 {
		result = c.callWithRateLimitRetry(callCtx, session, params, result, bare)
	}

	payload, marshalErr := encodeResult(result)
	if marshalErr != nil {
		return c.errorChunk(marshalErr.Error()), nil
	}

	// Cap the payload before it hits the assistant turn. A get_file_contents
	// returning a 50 MB blob would blow Gemini's context window; we replace
	// the oversized envelope with a truncated one the model can still
	// parse + understand was capped.
	if c.opts.MaxResultBytes > 0 && len(payload) > c.opts.MaxResultBytes {
		payload = truncatePayload(payload, c.opts.MaxResultBytes, result.IsError)
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

// callWithRateLimitRetry inspects result for a GitHub rate-limit
// failure and, if one is present, re-issues the same tool call up to
// c.opts.RateLimitRetries times with exponential backoff. Returns
// either the eventual success result, a fresh non-rate-limit failure,
// or the last rate-limit result if the budget was exhausted (so the
// model still sees the upstream's diagnostic message instead of a
// silent drop).
//
// Bumps nomaddev_github_rate_limit_retries_total per attempt
// (outcome=retried) and once more on the give-up path
// (outcome=gave_up) so dashboards can alert on either pattern.
func (c *Client) callWithRateLimitRetry(
	ctx context.Context, session *mcp.ClientSession,
	params *mcp.CallToolParams, first *mcp.CallToolResult, toolBare string,
) *mcp.CallToolResult {
	if !shouldRetryRateLimit(first) {
		return first
	}
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	base := c.opts.RateLimitBaseBackoff
	if base <= 0 {
		base = time.Second
	}
	result := first
	for attempt := 1; attempt <= c.opts.RateLimitRetries; attempt++ {
		hint := retryHintFromResult(result)
		wait := nextBackoff(attempt, base, hint, rng)
		c.logger.Warn("githubmcp: rate-limited, backing off",
			"tool", toolBare, "attempt", attempt, "wait", wait,
			"hint", hint, "max_retries", c.opts.RateLimitRetries)
		metrics.GitHubRateLimitRetriesTotal.WithLabelValues("retried").Inc()

		select {
		case <-time.After(wait):
		case <-ctx.Done():
			// Caller deadline / cancel wins over the retry budget.
			c.logger.Warn("githubmcp: rate-limit retry aborted by ctx",
				"tool", toolBare, "err", ctx.Err())
			metrics.GitHubRateLimitRetriesTotal.WithLabelValues("gave_up").Inc()
			return result
		}

		next, err := session.CallTool(ctx, params)
		if err != nil {
			// Transport error during a rate-limit retry: hand back
			// the original rate-limit result so the model sees the
			// useful message (and the transport error didn't actually
			// happen at the GitHub layer — likely ctx-bound here).
			return result
		}
		if !shouldRetryRateLimit(next) {
			// Recovered (either success or a different non-retryable
			// error). Surface the new result.
			return next
		}
		result = next
	}
	c.logger.Warn("githubmcp: rate-limit retries exhausted",
		"tool", toolBare, "retries", c.opts.RateLimitRetries)
	metrics.GitHubRateLimitRetriesTotal.WithLabelValues("gave_up").Inc()
	return result
}

// shouldRetryRateLimit walks the result content for any of the
// package's rate-limit markers (see ratelimit.go). Returns true only
// for results explicitly tagged IsError=true so a tool that
// legitimately returns prose containing the word "rate limit" — say,
// a GitHub Discussions search result — doesn't get retried.
func shouldRetryRateLimit(r *mcp.CallToolResult) bool {
	if r == nil || !r.IsError {
		return false
	}
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			if looksLikeRateLimitText(tc.Text) {
				return true
			}
		}
	}
	return false
}

// retryHintFromResult extracts the first Retry-After hint found in
// the result's text content, or 0 if none.
func retryHintFromResult(r *mcp.CallToolResult) time.Duration {
	if r == nil {
		return 0
	}
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			if d := parseRetryAfter(tc.Text); d > 0 {
				return d
			}
		}
	}
	return 0
}

// truncatePayload replaces an oversized encodeResult JSON with a smaller
// envelope that preserves a head-of-payload preview, the IsError flag, and
// machine-readable truncation metadata so the model can self-correct
// (typically by narrowing its request — e.g., asking for a smaller page or
// a specific path range).
//
// We deliberately don't try to walk the original JSON to selectively shrink
// text blocks: that's complex, error-prone, and the model already handles
// "<truncated>" markers correctly when it sees them. A single text block
// with the head bytes + bytes-omitted count is both simpler and more
// informative.
func truncatePayload(payload []byte, maxBytes int, wasError bool) []byte {
	// Reserve ~512 B for the envelope wrapper so the preview fits inside
	// maxBytes after re-marshaling.
	const envelopeOverhead = 512
	previewMax := maxBytes - envelopeOverhead
	if previewMax < 256 {
		previewMax = 256
	}
	preview := string(payload)
	if len(preview) > previewMax {
		preview = preview[:previewMax]
	}
	envelope := struct {
		Content []map[string]any `json:"content"`
		IsError bool             `json:"is_error,omitempty"`

		// Machine-readable truncation metadata. The model's tool-result
		// parser sees these as extra fields and ignores them; an operator
		// reading the assistant turn for debugging sees the cap.
		Truncated     bool `json:"truncated"`
		OriginalBytes int  `json:"original_bytes"`
		PreviewBytes  int  `json:"preview_bytes"`
	}{
		Content: []map[string]any{
			{
				"type": "text",
				"text": fmt.Sprintf(
					"[githubmcp: result truncated — original %d bytes, preview %d bytes; "+
						"retry with a narrower query (smaller page, specific path) for "+
						"a complete response]\n\n%s",
					len(payload), len(preview), preview,
				),
			},
		},
		IsError:       wasError,
		Truncated:     true,
		OriginalBytes: len(payload),
		PreviewBytes:  len(preview),
	}
	out, err := json.Marshal(envelope)
	if err != nil {
		// Last-resort fallback: a minimal text block. Should never trip
		// since the envelope shape is JSON-marshal-safe by construction.
		return []byte(`{"content":[{"type":"text","text":"[githubmcp: result truncated and re-marshal failed]"}],"truncated":true}`)
	}
	return out
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

// contextErrorChunk preserves the original context error (DeadlineExceeded
// or Canceled) so wsserver's classifyExit() routes it to SandboxErrTimeout
// or SandboxErrCanceled instead of the generic Internal bucket.
func (c *Client) contextErrorChunk(err error) <-chan sandbox.ExecChunk {
	ch := make(chan sandbox.ExecChunk, 2)
	ch <- sandbox.ExecChunk{Stream: sandbox.StreamExit, Err: err}
	close(ch)
	return ch
}

// errorChunkBadRequest wraps a free-text error with sandbox.ErrBadRequest so
// classifyExit routes it to event.SandboxErrBadRequest. Used for pre-flight
// argument validation (arg-size cap, future arg-shape checks) where the
// fault lies with the caller, not the subprocess.
func (c *Client) errorChunkBadRequest(msg string) <-chan sandbox.ExecChunk {
	ch := make(chan sandbox.ExecChunk, 2)
	ch <- sandbox.ExecChunk{
		Stream: sandbox.StreamExit,
		Err:    fmt.Errorf("%w: githubmcp: %s", sandbox.ErrBadRequest, msg),
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
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	var firstErr error
	if c.session != nil {
		if err := c.session.Close(); err != nil {
			firstErr = err
		}
		c.session = nil
	}
	if c.stderrDone != nil {
		select {
		case <-c.stderrDone:
		case <-time.After(2 * time.Second):
		}
		c.stderrDone = nil
	}
	if c.cmd != nil && c.cmd.Process != nil {
		// Belt-and-suspenders: ensure no zombie if Close raced something.
		_ = c.cmd.Process.Release()
	}
	c.cmd = nil
	return firstErr
}
