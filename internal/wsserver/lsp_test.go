package wsserver

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/mattcheramie/nomaddev/internal/event"
	"github.com/mattcheramie/nomaddev/internal/middleware"
)

// init turns this test binary into a fake stdio language server when launched
// with -nomaddev-fake-lsp, so the lsp_query handler tests run hermetically
// with no real gopls. The flag is checked before testing.Main parses flags.
func init() {
	for _, a := range os.Args[1:] {
		if a == "-nomaddev-fake-lsp" {
			runFakeLSPForWS()
			os.Exit(0)
		}
	}
}

func runFakeLSPForWS() {
	r := bufio.NewReader(os.Stdin)
	for {
		msg, err := fakeLSPReadFrame(r)
		if err != nil {
			return
		}
		method, _ := msg["method"].(string)
		id, hasID := msg["id"]
		if method == "exit" {
			os.Exit(0)
		}
		if !hasID {
			continue // notification (initialized, didOpen, …)
		}
		var result any
		switch method {
		case "initialize":
			result = map[string]any{"capabilities": map[string]any{}}
		case "textDocument/definition":
			cwd, _ := os.Getwd() // StartLSPServer sets cmd.Dir to the workspace root
			result = []map[string]any{{
				"uri": "file://" + filepath.Join(cwd, "target.go"),
				"range": map[string]any{
					"start": map[string]any{"line": 41, "character": 5},
					"end":   map[string]any{"line": 41, "character": 8},
				},
			}}
		default:
			result = nil
		}
		fakeLSPWriteFrame(os.Stdout, map[string]any{
			"jsonrpc": "2.0", "id": id, "result": result,
		})
	}
}

func fakeLSPWriteFrame(w io.Writer, v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(b))
	_, _ = w.Write(b)
}

func fakeLSPReadFrame(r *bufio.Reader) (map[string]any, error) {
	n := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if v, ok := strings.CutPrefix(line, "Content-Length:"); ok {
			n, _ = strconv.Atoi(strings.TrimSpace(v))
		}
	}
	if n < 0 {
		return nil, fmt.Errorf("frame missing Content-Length")
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(buf, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// fakeLSPServers returns the language->command table pointing gopls's slot at
// this test binary's fake-LSP mode. Skips when the binary path has whitespace
// (strings.Fields would mis-split the command line).
func fakeLSPServers(t *testing.T) map[string]string {
	t.Helper()
	if strings.ContainsAny(os.Args[0], " \t") {
		t.Skip("test binary path contains whitespace; skipping fake-LSP integration")
	}
	return map[string]string{"go": os.Args[0] + " -nomaddev-fake-lsp"}
}

// collectLSPResult drives one lsp_query command.request and returns the parsed
// JSON envelope plus the terminal command.result.
func collectLSPResult(t *testing.T, c *websocket.Conn) (string, event.CommandResultPayload) {
	t.Helper()
	var body string
	for i := 0; i < 20; i++ {
		env := readEnv(t, c)
		switch env.Type {
		case event.EventCommandChunk:
			var p event.CommandChunkPayload
			_ = env.UnmarshalPayload(&p)
			if p.Stream == event.StreamStdout {
				body += p.Data
			}
		case event.EventCommandResult:
			var p event.CommandResultPayload
			_ = env.UnmarshalPayload(&p)
			return body, p
		}
	}
	t.Fatal("lsp_query: no command.result observed")
	return "", event.CommandResultPayload{}
}

func TestLSPQuery_Definition_DirectCommand(t *testing.T) {
	servers := fakeLSPServers(t)
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "main.go"),
		[]byte("package main\n\nfunc Foo() { Bar() }\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	ts, srv, _, issuer := newTestServerFull(t, testOpts{LSPWorkspace: ws, LSPServers: servers})
	if srv.lsp == nil {
		t.Fatal("LSP registry not wired despite workspace + servers")
	}
	tok, _ := issuer.Sign("matt", "sess-lsp", nil)
	c, _, err := dialWithAuthHeader(t, ts, tok)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = readEnv(t, c) // hello

	req, _ := event.NewEnvelope(event.EventCommandRequest, event.CommandRequestPayload{
		Tool: middleware.ToolLSPQuery,
		Args: map[string]any{
			"operation": "definition",
			"path":      "main.go",
			"line":      3,
			"character": 14,
		},
	})
	writeEnv(t, c, req)

	body, result := collectLSPResult(t, c)
	if result.Error != "" {
		t.Fatalf("command.result error = %q (%s)", result.Error, result.ErrorMessage)
	}
	var env struct {
		Operation string `json:"operation"`
		Locations []struct {
			File string `json:"file"`
			Line int    `json:"line"`
		} `json:"locations"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("envelope JSON: %v (%q)", err, body)
	}
	if env.Operation != "definition" {
		t.Errorf("operation = %q, want definition", env.Operation)
	}
	if len(env.Locations) != 1 {
		t.Fatalf("locations = %d, want 1", len(env.Locations))
	}
	if filepath.Base(env.Locations[0].File) != "target.go" {
		t.Errorf("file = %q, want target.go", env.Locations[0].File)
	}
	if env.Locations[0].Line != 42 {
		t.Errorf("line = %d, want 42 (0-based 41 -> 1-based)", env.Locations[0].Line)
	}
}

func TestLSPQuery_NoWorkspace_ReturnsBadRequest(t *testing.T) {
	// newTestServer wires no workspace, so srv.lsp stays nil.
	ts, srv, _, issuer := newTestServer(t)
	if srv.lsp != nil {
		t.Fatal("LSP registry should be nil without a workspace")
	}
	tok, _ := issuer.Sign("matt", "sess-nolsp", nil)
	c, _, err := dialWithAuthHeader(t, ts, tok)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = readEnv(t, c) // hello

	req, _ := event.NewEnvelope(event.EventCommandRequest, event.CommandRequestPayload{
		Tool: middleware.ToolLSPQuery,
		Args: map[string]any{
			"operation": "definition", "path": "x.go", "line": 1, "character": 1,
		},
	})
	writeEnv(t, c, req)

	_, result := collectLSPResult(t, c)
	if result.Error != event.SandboxErrBadRequest {
		t.Fatalf("error code = %q, want %q", result.Error, event.SandboxErrBadRequest)
	}
	if !strings.Contains(result.ErrorMessage, "not available") {
		t.Errorf("error message = %q, want a 'not available' explanation", result.ErrorMessage)
	}
}

func TestLSPQuery_BadArgs_ReturnsBadRequest(t *testing.T) {
	servers := fakeLSPServers(t)
	ws := t.TempDir()
	ts, _, _, issuer := newTestServerFull(t, testOpts{LSPWorkspace: ws, LSPServers: servers})
	tok, _ := issuer.Sign("matt", "sess-badargs", nil)
	c, _, err := dialWithAuthHeader(t, ts, tok)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = readEnv(t, c) // hello

	// 'definition' with no line/character must fail validation.
	req, _ := event.NewEnvelope(event.EventCommandRequest, event.CommandRequestPayload{
		Tool: middleware.ToolLSPQuery,
		Args: map[string]any{"operation": "definition", "path": "main.go"},
	})
	writeEnv(t, c, req)

	_, result := collectLSPResult(t, c)
	if result.Error != event.SandboxErrBadRequest {
		t.Fatalf("error code = %q, want %q", result.Error, event.SandboxErrBadRequest)
	}
}
