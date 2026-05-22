package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// lsp.go backs the lsp_query tool: semantic code navigation answered by a
// real language server (gopls, typescript-language-server, pylsp). A language
// server indexes the workspace and answers Language Server Protocol operations
// — go-to-definition, find-references, hover, implementations, symbol search.
//
// Like monitor_daemon (see daemon.go), a language server is a long-lived
// process and so runs on the orchestrator HOST, not inside a one-shot
// ephemeral container — a container is force-removed the instant its exec
// returns and cannot host a daemon. The server reads the same workspace
// directory the Docker runner bind-mounts. An lsp_query *call* is synchronous
// request/response; only the server *process* is long-lived and reused across
// calls via the LSPRegistry.

const (
	// lspStopGrace is how long Shutdown waits after SIGTERM before SIGKILL.
	lspStopGrace = 5 * time.Second
	// defaultLSPInitTimeout bounds the initialize handshake. A language
	// server indexes the workspace here, so the budget is generous.
	defaultLSPInitTimeout = 60 * time.Second
	// defaultLSPIdleTimeout reclaims a server with no recent query.
	defaultLSPIdleTimeout = 5 * time.Minute
	// lspIdleScanInterval is how often the registry's reaper runs.
	lspIdleScanInterval = time.Minute
)

// ErrLSPUnavailable is returned when no language server is configured for a
// language or the server binary could not be started.
var ErrLSPUnavailable = errors.New("sandbox: language server unavailable")

// lspMessage is one JSON-RPC 2.0 frame, covering requests, responses and
// notifications. ID is a RawMessage because a server-to-client request may
// carry either a number or a string id; our own requests always use numbers.
type lspMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *lspError       `json:"error,omitempty"`
}

type lspError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// writeLSPFrame writes one Content-Length-framed JSON-RPC message.
func writeLSPFrame(w io.Writer, msg any) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

// readLSPFrame reads one Content-Length-framed JSON-RPC message body. It
// parses the header block, then reads exactly Content-Length bytes.
func readLSPFrame(r *bufio.Reader) ([]byte, error) {
	contentLength := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break // blank line terminates the header block
		}
		if v, ok := strings.CutPrefix(line, "Content-Length:"); ok {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return nil, fmt.Errorf("lsp: bad Content-Length %q: %w", v, err)
			}
			contentLength = n
		}
		// Other headers (e.g. Content-Type) are ignored.
	}
	if contentLength < 0 {
		return nil, errors.New("lsp: frame missing Content-Length header")
	}
	body := make([]byte, contentLength)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

// LSPServer is a handle on one running language-server process. It speaks
// JSON-RPC over the process's stdio and demultiplexes responses by id.
type LSPServer struct {
	Lang      string
	Command   string
	StartedAt time.Time

	cmd     *exec.Cmd
	pgid    int
	stdin   io.WriteCloser
	rootDir string // absolute workspace root

	writeMu sync.Mutex // serializes frame writes

	mu       sync.Mutex
	nextID   int64
	pending  map[int64]chan lspMessage
	openDocs map[string]bool // didOpen-ed document uris
	lastUsed time.Time
	closed   bool

	done     chan struct{} // closed when the server is dead or shutting down
	doneOnce sync.Once
	stopOnce sync.Once
}

// StartLSPServer spawns the language server in argv, runs the LSP initialize
// handshake rooted at rootDir, and returns a ready server. The process runs
// in its own group (Setpgid) so Shutdown can signal the whole group.
func StartLSPServer(lang string, argv []string, rootDir string, initTimeout time.Duration) (*LSPServer, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("%w: no server command for %q", ErrLSPUnavailable, lang)
	}
	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, fmt.Errorf("lsp: resolve workspace root: %w", err)
	}
	if initTimeout <= 0 {
		initTimeout = defaultLSPInitTimeout
	}

	// G204: launching an operator-configured language server is the entire
	// purpose of this tool.
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec
	cmd.Dir = absRoot
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Own os.Pipe pairs (not StdoutPipe): cmd.Wait closes a StdoutPipe on
	// process exit, which would race the reader. A plain *os.File is left
	// alone, so the reader drains cleanly. Same rationale as StartDaemon.
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("lsp: stdin pipe: %w", err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		_ = inR.Close()
		_ = inW.Close()
		return nil, fmt.Errorf("lsp: stdout pipe: %w", err)
	}
	cmd.Stdin = inR
	cmd.Stdout = outW
	// Stderr left nil — the child's stderr goes to the null device.

	if err := cmd.Start(); err != nil {
		for _, f := range []*os.File{inR, inW, outR, outW} {
			_ = f.Close()
		}
		return nil, fmt.Errorf("%w: start %q: %v", ErrLSPUnavailable, argv[0], err)
	}
	// Drop the parent's copy of the child's ends so EOF fires on child exit.
	_ = inR.Close()
	_ = outW.Close()

	s := &LSPServer{
		Lang:      lang,
		Command:   strings.Join(argv, " "),
		StartedAt: time.Now().UTC(),
		cmd:       cmd,
		pgid:      cmd.Process.Pid, // Setpgid: the child leads a group whose id is its pid
		stdin:     inW,
		rootDir:   absRoot,
		pending:   make(map[int64]chan lspMessage),
		openDocs:  make(map[string]bool),
		lastUsed:  time.Now(),
		done:      make(chan struct{}),
	}
	go s.readLoop(outR)
	go func() { _ = cmd.Wait() }() // reap the process

	ctx, cancel := context.WithTimeout(context.Background(), initTimeout)
	defer cancel()
	if err := s.initialize(ctx); err != nil {
		s.Shutdown()
		return nil, fmt.Errorf("%w: %v", ErrLSPUnavailable, err)
	}
	return s, nil
}

// readLoop demultiplexes inbound frames until the pipe closes (process exit).
func (s *LSPServer) readLoop(stdout io.ReadCloser) {
	defer func() {
		_ = stdout.Close()
		s.closeDone()
	}()
	r := bufio.NewReaderSize(stdout, 64*1024)
	for {
		body, err := readLSPFrame(r)
		if err != nil {
			return
		}
		var msg lspMessage
		if err := json.Unmarshal(body, &msg); err != nil {
			continue
		}
		switch {
		case len(msg.ID) > 0 && msg.Method != "":
			// Server-to-client request: it MUST be answered or the server
			// (gopls especially) can stall waiting on the reply.
			s.replyToServerRequest(msg)
		case len(msg.ID) > 0:
			s.routeResponse(msg)
		default:
			// Notification ($/progress, window/logMessage, …) — ignored.
		}
	}
}

// replyToServerRequest answers a server-initiated request. workspace/configuration
// expects an array sized to params.items; every other request is satisfied with
// a null result.
func (s *LSPServer) replyToServerRequest(msg lspMessage) {
	var result any
	if msg.Method == "workspace/configuration" {
		var p struct {
			Items []json.RawMessage `json:"items"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		result = make([]any, len(p.Items)) // array of nulls
	}
	_ = s.writeFrame(map[string]any{
		"jsonrpc": "2.0",
		"id":      msg.ID,
		"result":  result,
	})
}

// routeResponse delivers a response to the waiting Request call.
func (s *LSPServer) routeResponse(msg lspMessage) {
	var id int64
	if err := json.Unmarshal(msg.ID, &id); err != nil {
		return
	}
	s.mu.Lock()
	ch, ok := s.pending[id]
	if ok {
		delete(s.pending, id)
	}
	s.mu.Unlock()
	if ok {
		ch <- msg // ch is buffered (size 1); exactly one response per id
	}
}

func (s *LSPServer) writeFrame(msg any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return writeLSPFrame(s.stdin, msg)
}

func (s *LSPServer) closeDone() {
	s.doneOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		close(s.done)
	})
}

func (s *LSPServer) isClosed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *LSPServer) touch() {
	s.mu.Lock()
	s.lastUsed = time.Now()
	s.mu.Unlock()
}

func (s *LSPServer) idleFor() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.lastUsed)
}

// Request issues a JSON-RPC request and blocks until the response, the
// context, or process death. An LSP error response is returned as a Go error.
func (s *LSPServer) Request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, fmt.Errorf("lsp: server for %q is not running", s.Lang)
	}
	s.nextID++
	id := s.nextID
	ch := make(chan lspMessage, 1)
	s.pending[id] = ch
	s.lastUsed = time.Now()
	s.mu.Unlock()

	cleanup := func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}

	if err := s.writeFrame(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}); err != nil {
		cleanup()
		return nil, fmt.Errorf("lsp: write %s: %w", method, err)
	}

	select {
	case <-ctx.Done():
		cleanup()
		return nil, ctx.Err()
	case <-s.done:
		cleanup()
		return nil, fmt.Errorf("lsp: server exited before answering %s", method)
	case msg := <-ch:
		if msg.Error != nil {
			return nil, fmt.Errorf("lsp: %s: %s (code %d)", method, msg.Error.Message, msg.Error.Code)
		}
		return msg.Result, nil
	}
}

// Notify sends a JSON-RPC notification (no response expected).
func (s *LSPServer) Notify(method string, params any) error {
	return s.writeFrame(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
}

func (s *LSPServer) initialize(ctx context.Context) error {
	rootURI := pathToURI(s.rootDir)
	params := map[string]any{
		"processId": os.Getpid(),
		"rootUri":   rootURI,
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"synchronization": map[string]any{},
				"definition":      map[string]any{"linkSupport": true},
				"references":      map[string]any{},
				"hover":           map[string]any{"contentFormat": []string{"markdown", "plaintext"}},
				"implementation":  map[string]any{"linkSupport": true},
				"documentSymbol":  map[string]any{"hierarchicalDocumentSymbolSupport": true},
			},
			"workspace": map[string]any{
				"symbol":           map[string]any{},
				"configuration":    true,
				"workspaceFolders": true,
			},
		},
		"workspaceFolders": []map[string]any{
			{"uri": rootURI, "name": filepath.Base(s.rootDir)},
		},
	}
	if _, err := s.Request(ctx, "initialize", params); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if err := s.Notify("initialized", map[string]any{}); err != nil {
		return fmt.Errorf("initialized: %w", err)
	}
	return nil
}

// ensureOpen sends textDocument/didOpen for absPath the first time it is
// queried. Servers resolve a position against the in-memory document, so the
// file must be opened before a position request.
func (s *LSPServer) ensureOpen(absPath string) error {
	uri := pathToURI(absPath)
	s.mu.Lock()
	opened := s.openDocs[uri]
	s.mu.Unlock()
	if opened {
		return nil
	}
	content, err := os.ReadFile(absPath) //nolint:gosec // path validated by the caller
	if err != nil {
		return fmt.Errorf("read %s: %w", filepath.Base(absPath), err)
	}
	langID := languageIDForPath(absPath)
	if langID == "" {
		langID = s.Lang
	}
	if err := s.Notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri":        uri,
			"languageId": langID,
			"version":    1,
			"text":       string(content),
		},
	}); err != nil {
		return err
	}
	s.mu.Lock()
	s.openDocs[uri] = true
	s.mu.Unlock()
	return nil
}

// Shutdown gracefully stops the server (LSP shutdown + exit) then terminates
// its process group. Idempotent.
func (s *LSPServer) Shutdown() {
	s.stopOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = s.Request(ctx, "shutdown", nil)
		cancel()
		_ = s.Notify("exit", nil)
		s.closeDone()
		_ = syscall.Kill(-s.pgid, syscall.SIGTERM)
		go func() {
			time.Sleep(lspStopGrace)
			_ = syscall.Kill(-s.pgid, syscall.SIGKILL)
		}()
		_ = s.stdin.Close()
	})
}

// LSPQuery is one navigation request against a language server.
type LSPQuery struct {
	Operation          string // definition|references|hover|implementation|document_symbols|workspace_symbols
	AbsPath            string // absolute file path; empty for workspace_symbols
	Line               int    // 1-based
	Character          int    // 1-based
	Symbol             string // query string for workspace_symbols
	IncludeDeclaration bool   // references only
}

// Query runs one operation and returns the marshaled result envelope. A
// transport failure (timeout, process death) is returned as a Go error; an
// LSP method error or a file-read failure is folded into the envelope's
// "error" field so the model can self-correct.
func (s *LSPServer) Query(ctx context.Context, q LSPQuery, maxBytes int) ([]byte, error) {
	res := lspQueryResult{Operation: q.Operation, Locations: []lspLocation{}}
	raw, err := s.execute(ctx, q)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return nil, err
		}
		res.Error = err.Error()
		return marshalLSPResult(res, maxBytes)
	}
	switch q.Operation {
	case "definition", "implementation", "references":
		res.Locations = parseLocations(raw, s.rootDir)
	case "hover":
		res.Hover = parseHover(raw)
	case "document_symbols", "workspace_symbols":
		res.Symbols = parseSymbols(raw, s.rootDir, q.AbsPath)
	}
	res.Total = len(res.Locations) + len(res.Symbols)
	res.Returned = res.Total
	return marshalLSPResult(res, maxBytes)
}

// execute opens the target document if needed and issues the LSP request,
// returning the raw result payload.
func (s *LSPServer) execute(ctx context.Context, q LSPQuery) (json.RawMessage, error) {
	if q.AbsPath != "" {
		if err := s.ensureOpen(q.AbsPath); err != nil {
			return nil, err
		}
	}
	s.touch()
	switch q.Operation {
	case "definition":
		return s.Request(ctx, "textDocument/definition", s.posParams(q))
	case "implementation":
		return s.Request(ctx, "textDocument/implementation", s.posParams(q))
	case "hover":
		return s.Request(ctx, "textDocument/hover", s.posParams(q))
	case "references":
		p := s.posParams(q)
		p["context"] = map[string]any{"includeDeclaration": q.IncludeDeclaration}
		return s.Request(ctx, "textDocument/references", p)
	case "document_symbols":
		return s.Request(ctx, "textDocument/documentSymbol", map[string]any{
			"textDocument": map[string]any{"uri": pathToURI(q.AbsPath)},
		})
	case "workspace_symbols":
		return s.Request(ctx, "workspace/symbol", map[string]any{"query": q.Symbol})
	}
	return nil, fmt.Errorf("%w: unknown lsp operation %q", ErrBadRequest, q.Operation)
}

// posParams builds the {textDocument, position} pair, converting the tool's
// 1-based coordinates to LSP's 0-based positions.
func (s *LSPServer) posParams(q LSPQuery) map[string]any {
	return map[string]any{
		"textDocument": map[string]any{"uri": pathToURI(q.AbsPath)},
		"position":     map[string]any{"line": q.Line - 1, "character": q.Character - 1},
	}
}

// lspQueryResult is the stable envelope returned on stdout. File paths are
// workspace-relative and positions are 1-based, matching searchSyntaxResult.
type lspQueryResult struct {
	Operation string        `json:"operation"`
	Locations []lspLocation `json:"locations"`
	Symbols   []lspSymbol   `json:"symbols,omitempty"`
	Hover     string        `json:"hover,omitempty"`
	Total     int           `json:"total"`
	Returned  int           `json:"returned"`
	Truncated bool          `json:"truncated,omitempty"`
	Error     string        `json:"error,omitempty"`
}

type lspLocation struct {
	File      string `json:"file"`
	Line      int    `json:"line"`
	Column    int    `json:"column"`
	EndLine   int    `json:"end_line,omitempty"`
	EndColumn int    `json:"end_column,omitempty"`
}

type lspSymbol struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	File      string `json:"file"`
	Line      int    `json:"line"`
	Column    int    `json:"column"`
	Container string `json:"container,omitempty"`
}

// LSP wire shapes we project from. A definition result element is either a
// Location ({uri,range}) or a LocationLink ({targetUri,targetRange}); the
// combined struct reads whichever is populated.
type rawRange struct {
	Start rawPos `json:"start"`
	End   rawPos `json:"end"`
}

type rawPos struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type rawLocation struct {
	URI         string   `json:"uri"`
	Range       rawRange `json:"range"`
	TargetURI   string   `json:"targetUri"`
	TargetRange rawRange `json:"targetRange"`
}

type rawSymbol struct {
	Name           string      `json:"name"`
	Kind           int         `json:"kind"`
	ContainerName  string      `json:"containerName"`
	Location       *rawLocSym  `json:"location"`       // SymbolInformation / WorkspaceSymbol
	Range          *rawRange   `json:"range"`          // DocumentSymbol
	SelectionRange *rawRange   `json:"selectionRange"` // DocumentSymbol
	Children       []rawSymbol `json:"children"`       // DocumentSymbol
}

type rawLocSym struct {
	URI   string   `json:"uri"`
	Range rawRange `json:"range"`
}

// parseLocations projects a definition/references/implementation result
// (single object, array, Location or LocationLink) into lspLocation values.
func parseLocations(raw json.RawMessage, root string) []lspLocation {
	out := []lspLocation{}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return out
	}
	var items []json.RawMessage
	if trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return out
		}
	} else {
		items = []json.RawMessage{trimmed}
	}
	for _, it := range items {
		var rl rawLocation
		if err := json.Unmarshal(it, &rl); err != nil {
			continue
		}
		uri, rng := rl.URI, rl.Range
		if uri == "" {
			uri, rng = rl.TargetURI, rl.TargetRange
		}
		if uri == "" {
			continue
		}
		out = append(out, lspLocation{
			File:      relPath(root, uriToPath(uri)),
			Line:      rng.Start.Line + 1,
			Column:    rng.Start.Character + 1,
			EndLine:   rng.End.Line + 1,
			EndColumn: rng.End.Character + 1,
		})
	}
	return out
}

// parseHover flattens an LSP Hover.contents (MarkupContent, MarkedString, or a
// list of either) into plain text.
func parseHover(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return ""
	}
	var h struct {
		Contents json.RawMessage `json:"contents"`
	}
	if err := json.Unmarshal(trimmed, &h); err != nil {
		return ""
	}
	return flattenHoverContents(h.Contents)
}

func flattenHoverContents(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	switch raw[0] {
	case '"':
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	case '{':
		var o struct {
			Value string `json:"value"`
		}
		_ = json.Unmarshal(raw, &o)
		return o.Value
	case '[':
		var arr []json.RawMessage
		_ = json.Unmarshal(raw, &arr)
		parts := make([]string, 0, len(arr))
		for _, e := range arr {
			if v := flattenHoverContents(e); v != "" {
				parts = append(parts, v)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// parseSymbols projects a documentSymbol / workspace symbol result, flattening
// any DocumentSymbol child trees. queriedAbsPath is the file backing a
// document_symbols call (DocumentSymbol entries carry no uri of their own).
func parseSymbols(raw json.RawMessage, root, queriedAbsPath string) []lspSymbol {
	out := []lspSymbol{}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" || trimmed[0] != '[' {
		return out
	}
	var items []rawSymbol
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return out
	}
	var walk func(syms []rawSymbol, container string)
	walk = func(syms []rawSymbol, container string) {
		for _, sym := range syms {
			file := relPath(root, queriedAbsPath)
			var line, col int
			switch {
			case sym.Location != nil:
				file = relPath(root, uriToPath(sym.Location.URI))
				line = sym.Location.Range.Start.Line + 1
				col = sym.Location.Range.Start.Character + 1
			case sym.SelectionRange != nil:
				line = sym.SelectionRange.Start.Line + 1
				col = sym.SelectionRange.Start.Character + 1
			case sym.Range != nil:
				line = sym.Range.Start.Line + 1
				col = sym.Range.Start.Character + 1
			}
			cont := container
			if sym.ContainerName != "" {
				cont = sym.ContainerName
			}
			out = append(out, lspSymbol{
				Name:      sym.Name,
				Kind:      symbolKindName(sym.Kind),
				File:      file,
				Line:      line,
				Column:    col,
				Container: cont,
			})
			if len(sym.Children) > 0 {
				walk(sym.Children, sym.Name)
			}
		}
	}
	walk(items, "")
	return out
}

// marshalLSPResult marshals the envelope, dropping trailing entries until it
// fits under maxBytes (0 disables the cap) — the searchSyntaxResult pattern.
func marshalLSPResult(res lspQueryResult, maxBytes int) ([]byte, error) {
	if res.Locations == nil {
		res.Locations = []lspLocation{}
	}
	body, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	if maxBytes <= 0 || len(body) <= maxBytes {
		return body, nil
	}
	res.Truncated = true
	for len(body) > maxBytes && (len(res.Symbols) > 0 || len(res.Locations) > 0) {
		if len(res.Symbols) > 0 {
			res.Symbols = res.Symbols[:len(res.Symbols)-1]
		} else {
			res.Locations = res.Locations[:len(res.Locations)-1]
		}
		res.Returned = len(res.Locations) + len(res.Symbols)
		if body, err = json.Marshal(res); err != nil {
			return nil, err
		}
	}
	return body, nil
}

// LSPRegistry owns the language servers of every session: it lazily starts a
// server on first use, reuses the warm process for later queries, reaps idle
// servers, and tears every server down when a session disconnects. It is the
// LSP counterpart of DaemonRegistry and is safe for concurrent use.
type LSPRegistry struct {
	servers      map[string][]string // language -> server argv
	workspaceDir string              // absolute workspace root
	perSession   bool
	initTimeout  time.Duration
	idleTimeout  time.Duration
	log          *slog.Logger

	mu      sync.Mutex
	entries map[string]*lspEntry // "sid\x00lang" -> entry
	stopCh  chan struct{}
}

// lspEntry guards one server slot. Its own mutex serializes a cold start for
// a given session+language without blocking the whole registry.
type lspEntry struct {
	mu        sync.Mutex
	sessionID string
	server    *LSPServer
}

// LSPRegistryConfig configures NewLSPRegistry.
type LSPRegistryConfig struct {
	// Servers maps a language id to its server command line, e.g.
	// {"go": "gopls", "python": "pylsp"}.
	Servers      map[string]string
	WorkspaceDir string
	PerSession   bool
	InitTimeout  time.Duration
	IdleTimeout  time.Duration
	Logger       *slog.Logger
}

// DefaultLSPServers is the built-in language -> server command-line table.
func DefaultLSPServers() map[string]string {
	return map[string]string{
		"go":         "gopls",
		"typescript": "typescript-language-server --stdio",
		"javascript": "typescript-language-server --stdio",
		"python":     "pylsp",
	}
}

// NewLSPRegistry constructs a registry and starts its idle reaper.
func NewLSPRegistry(cfg LSPRegistryConfig) *LSPRegistry {
	servers := make(map[string][]string, len(cfg.Servers))
	for lang, cmdline := range cfg.Servers {
		if argv := strings.Fields(cmdline); len(argv) > 0 {
			servers[lang] = argv
		}
	}
	absWS := cfg.WorkspaceDir
	if abs, err := filepath.Abs(cfg.WorkspaceDir); err == nil {
		absWS = abs
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	idle := cfg.IdleTimeout
	if idle <= 0 {
		idle = defaultLSPIdleTimeout
	}
	r := &LSPRegistry{
		servers:      servers,
		workspaceDir: absWS,
		perSession:   cfg.PerSession,
		initTimeout:  cfg.InitTimeout,
		idleTimeout:  idle,
		log:          log,
		entries:      make(map[string]*lspEntry),
		stopCh:       make(chan struct{}),
	}
	go r.reapLoop()
	return r
}

// rootFor returns the absolute workspace root a session's server is rooted at.
func (r *LSPRegistry) rootFor(sessionID string) string {
	if r.perSession && sessionID != "" {
		return filepath.Join(r.workspaceDir, sanitizeSID(sessionID))
	}
	return r.workspaceDir
}

// ResolvePath joins a workspace-relative path onto the session's workspace
// root, rejecting absolute paths and any '..' escape.
func (r *LSPRegistry) ResolvePath(sessionID, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("%w: empty path", ErrBadRequest)
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: path must be relative to the workspace root", ErrBadRequest)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: path must not contain '..'", ErrBadRequest)
	}
	return filepath.Join(r.rootFor(sessionID), clean), nil
}

// GetOrStart returns a ready language server for the session+language,
// starting one (and indexing the workspace) on first use.
func (r *LSPRegistry) GetOrStart(sessionID, lang string) (*LSPServer, error) {
	argv, ok := r.servers[lang]
	if !ok {
		return nil, fmt.Errorf("%w: no language server configured for %q", ErrLSPUnavailable, lang)
	}
	key := sessionID + "\x00" + lang
	r.mu.Lock()
	e := r.entries[key]
	if e == nil {
		e = &lspEntry{sessionID: sessionID}
		r.entries[key] = e
	}
	r.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.server != nil && !e.server.isClosed() {
		return e.server, nil
	}
	srv, err := StartLSPServer(lang, argv, r.rootFor(sessionID), r.initTimeout)
	if err != nil {
		return nil, err
	}
	e.server = srv
	r.log.Info("lsp: started language server", "lang", lang, "sid", sessionID)
	return srv, nil
}

// StopAllForSession shuts down every language server a session started and
// returns the count stopped. Called on WebSocket teardown.
func (r *LSPRegistry) StopAllForSession(sessionID string) int {
	r.mu.Lock()
	var targets []*lspEntry
	for key, e := range r.entries {
		if e.sessionID == sessionID {
			targets = append(targets, e)
			delete(r.entries, key)
		}
	}
	r.mu.Unlock()

	stopped := 0
	for _, e := range targets {
		e.mu.Lock()
		if e.server != nil {
			e.server.Shutdown()
			e.server = nil
			stopped++
		}
		e.mu.Unlock()
	}
	return stopped
}

// Close stops the reaper and shuts every server down.
func (r *LSPRegistry) Close() {
	select {
	case <-r.stopCh:
	default:
		close(r.stopCh)
	}
	r.mu.Lock()
	entries := r.entries
	r.entries = make(map[string]*lspEntry)
	r.mu.Unlock()
	for _, e := range entries {
		e.mu.Lock()
		if e.server != nil {
			e.server.Shutdown()
			e.server = nil
		}
		e.mu.Unlock()
	}
}

func (r *LSPRegistry) reapLoop() {
	ticker := time.NewTicker(lspIdleScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			r.reapIdle()
		}
	}
}

// reapIdle shuts down servers that have been idle past idleTimeout, plus any
// whose process has already died.
func (r *LSPRegistry) reapIdle() {
	r.mu.Lock()
	snapshot := make(map[string]*lspEntry, len(r.entries))
	for k, e := range r.entries {
		snapshot[k] = e
	}
	r.mu.Unlock()

	for key, e := range snapshot {
		e.mu.Lock()
		srv := e.server
		dead := srv != nil && (srv.isClosed() || srv.idleFor() > r.idleTimeout)
		if dead {
			srv.Shutdown()
			e.server = nil
		}
		e.mu.Unlock()
		if dead {
			r.mu.Lock()
			if cur := r.entries[key]; cur == e && cur.server == nil {
				delete(r.entries, key)
			}
			r.mu.Unlock()
			r.log.Info("lsp: reaped idle language server", "lang", srv.Lang, "sid", e.sessionID)
		}
	}
}

// --- helpers --------------------------------------------------------------

// pathToURI renders an absolute path as a file:// URI.
func pathToURI(p string) string {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(p)}
	return u.String()
}

// uriToPath extracts the filesystem path from a file:// URI.
func uriToPath(uri string) string {
	if u, err := url.Parse(uri); err == nil && u.Path != "" {
		return filepath.FromSlash(u.Path)
	}
	return filepath.FromSlash(strings.TrimPrefix(uri, "file://"))
}

// relPath makes abs relative to root, falling back to abs when it escapes.
func relPath(root, abs string) string {
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return abs
	}
	return rel
}

// languageIDForPath returns the LSP languageId for a file, distinguishing the
// React variants the TypeScript/JavaScript language servers expect. An empty
// return means the extension is unknown; the caller falls back to the
// server's coarse language key.
func languageIDForPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".ts", ".mts", ".cts":
		return "typescript"
	case ".tsx":
		return "typescriptreact"
	case ".js", ".mjs", ".cjs":
		return "javascript"
	case ".jsx":
		return "javascriptreact"
	case ".py", ".pyi":
		return "python"
	default:
		return ""
	}
}

// LangFromPath infers a registry language key from a file extension.
func LangFromPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".ts", ".tsx", ".mts", ".cts":
		return "typescript"
	case ".js", ".jsx", ".mjs", ".cjs":
		return "javascript"
	case ".py", ".pyi":
		return "python"
	default:
		return ""
	}
}

// symbolKindName maps an LSP SymbolKind integer to a readable name.
func symbolKindName(kind int) string {
	names := []string{
		"", "file", "module", "namespace", "package", "class", "method",
		"property", "field", "constructor", "enum", "interface", "function",
		"variable", "constant", "string", "number", "boolean", "array",
		"object", "key", "null", "enum_member", "struct", "event",
		"operator", "type_parameter",
	}
	if kind >= 1 && kind < len(names) {
		return names[kind]
	}
	return "unknown"
}
