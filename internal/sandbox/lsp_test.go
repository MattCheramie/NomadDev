package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// init turns this test binary into a fake stdio language server when launched
// with the -nomaddev-fake-lsp flag. StartLSPServer re-execs os.Args[0] with
// that flag, so the LSP integration tests run hermetically with no real
// gopls. The flag is checked before testing.Main parses flags, so the unknown
// flag never reaches the flag package.
func init() {
	for _, a := range os.Args[1:] {
		if a == "-nomaddev-fake-lsp" {
			runFakeLSPServer()
			os.Exit(0)
		}
	}
}

// runFakeLSPServer speaks just enough JSON-RPC to satisfy the lsp_query path.
func runFakeLSPServer() {
	r := bufio.NewReader(os.Stdin)
	for {
		body, err := readLSPFrame(r)
		if err != nil {
			return
		}
		var msg lspMessage
		if json.Unmarshal(body, &msg) != nil {
			continue
		}
		if msg.Method == "" {
			continue // a reply to one of our server->client requests
		}
		if len(msg.ID) == 0 {
			if msg.Method == "exit" {
				os.Exit(0)
			}
			continue // notification (initialized, didOpen, …)
		}
		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{"capabilities": map[string]any{}}
		case "textDocument/definition", "textDocument/implementation":
			result = []map[string]any{fakeLoc("def.go", 9, 4, 9, 7)}
		case "textDocument/references":
			result = []map[string]any{
				fakeLoc("a.go", 1, 0, 1, 3),
				fakeLoc("b.go", 4, 2, 4, 5),
			}
		case "textDocument/hover":
			result = map[string]any{
				"contents": map[string]any{"kind": "markdown", "value": "func Foo()"},
			}
		case "textDocument/documentSymbol":
			result = []map[string]any{{
				"name":           "Foo",
				"kind":           12,
				"range":          fakeRange(2, 0, 5, 1),
				"selectionRange": fakeRange(2, 5, 2, 8),
				"children": []map[string]any{{
					"name":           "bar",
					"kind":           13,
					"range":          fakeRange(3, 1, 3, 9),
					"selectionRange": fakeRange(3, 1, 3, 4),
				}},
			}}
		case "workspace/symbol":
			result = []map[string]any{{
				"name":     "Foo",
				"kind":     12,
				"location": fakeLoc("def.go", 9, 4, 9, 7),
			}}
		case "shutdown":
			result = nil
		default:
			result = nil
		}
		_ = writeLSPFrame(os.Stdout, map[string]any{
			"jsonrpc": "2.0", "id": msg.ID, "result": result,
		})
	}
}

func fakeRange(sl, sc, el, ec int) map[string]any {
	return map[string]any{
		"start": map[string]any{"line": sl, "character": sc},
		"end":   map[string]any{"line": el, "character": ec},
	}
}

func fakeLoc(name string, sl, sc, el, ec int) map[string]any {
	cwd, _ := os.Getwd() // StartLSPServer sets cmd.Dir to the workspace root
	return map[string]any{
		"uri":   pathToURI(filepath.Join(cwd, name)),
		"range": fakeRange(sl, sc, el, ec),
	}
}

// --- framing --------------------------------------------------------------

func TestLSP_FrameRoundTrip(t *testing.T) {
	var buf strings.Builder
	want := map[string]any{"jsonrpc": "2.0", "method": "initialized"}
	if err := writeLSPFrame(&buf, want); err != nil {
		t.Fatalf("writeLSPFrame: %v", err)
	}
	got, err := readLSPFrame(bufio.NewReader(strings.NewReader(buf.String())))
	if err != nil {
		t.Fatalf("readLSPFrame: %v", err)
	}
	var msg lspMessage
	if err := json.Unmarshal(got, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.Method != "initialized" {
		t.Fatalf("method = %q, want initialized", msg.Method)
	}
}

func TestLSP_ReadFrame_MultipleFrames(t *testing.T) {
	var buf strings.Builder
	_ = writeLSPFrame(&buf, map[string]any{"id": 1})
	_ = writeLSPFrame(&buf, map[string]any{"id": 2})
	r := bufio.NewReader(strings.NewReader(buf.String()))
	for want := 1; want <= 2; want++ {
		body, err := readLSPFrame(r)
		if err != nil {
			t.Fatalf("frame %d: %v", want, err)
		}
		var m struct {
			ID int `json:"id"`
		}
		_ = json.Unmarshal(body, &m)
		if m.ID != want {
			t.Fatalf("frame id = %d, want %d", m.ID, want)
		}
	}
}

func TestLSP_ReadFrame_MissingContentLength(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("Content-Type: x\r\n\r\n{}"))
	if _, err := readLSPFrame(r); err == nil {
		t.Fatal("want error for frame without Content-Length")
	}
}

// --- server-to-client request replies -------------------------------------

func TestLSP_ReplyToServerRequest_Configuration(t *testing.T) {
	pr, pw := io.Pipe()
	s := &LSPServer{stdin: pw}
	go s.replyToServerRequest(lspMessage{
		ID:     json.RawMessage(`"cfg-1"`),
		Method: "workspace/configuration",
		Params: json.RawMessage(`{"items":[{"section":"gopls"},{"section":"go"}]}`),
	})
	body, err := readLSPFrame(bufio.NewReader(pr))
	if err != nil {
		t.Fatalf("readLSPFrame: %v", err)
	}
	var reply struct {
		ID     json.RawMessage   `json:"id"`
		Result []json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	if string(reply.ID) != `"cfg-1"` {
		t.Fatalf("reply id = %s, want \"cfg-1\"", reply.ID)
	}
	if len(reply.Result) != 2 {
		t.Fatalf("workspace/configuration result len = %d, want 2", len(reply.Result))
	}
}

func TestLSP_ReplyToServerRequest_GenericNull(t *testing.T) {
	pr, pw := io.Pipe()
	s := &LSPServer{stdin: pw}
	go s.replyToServerRequest(lspMessage{
		ID:     json.RawMessage(`7`),
		Method: "client/registerCapability",
	})
	body, err := readLSPFrame(bufio.NewReader(pr))
	if err != nil {
		t.Fatalf("readLSPFrame: %v", err)
	}
	if !strings.Contains(string(body), `"result":null`) {
		t.Fatalf("generic server request reply = %s, want null result", body)
	}
}

// --- result projection -----------------------------------------------------

func TestLSP_ParseLocations(t *testing.T) {
	root := "/work"
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"null", `null`, 0},
		{"empty array", `[]`, 0},
		{"single Location", `{"uri":"file:///work/a.go","range":{"start":{"line":2,"character":1},"end":{"line":2,"character":4}}}`, 1},
		{"Location array", `[{"uri":"file:///work/a.go","range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}}},{"uri":"file:///work/b.go","range":{"start":{"line":1,"character":0},"end":{"line":1,"character":1}}}]`, 2},
		{"LocationLink", `[{"targetUri":"file:///work/c.go","targetRange":{"start":{"line":5,"character":2},"end":{"line":5,"character":9}}}]`, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseLocations(json.RawMessage(c.raw), root)
			if len(got) != c.want {
				t.Fatalf("parseLocations len = %d, want %d", len(got), c.want)
			}
		})
	}
}

func TestLSP_ParseLocations_OneBasedAndRelative(t *testing.T) {
	raw := `{"uri":"file:///work/pkg/a.go","range":{"start":{"line":9,"character":4},"end":{"line":9,"character":7}}}`
	got := parseLocations(json.RawMessage(raw), "/work")
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	loc := got[0]
	if loc.File != "pkg/a.go" {
		t.Errorf("File = %q, want pkg/a.go", loc.File)
	}
	if loc.Line != 10 || loc.Column != 5 {
		t.Errorf("position = %d:%d, want 10:5 (1-based)", loc.Line, loc.Column)
	}
}

func TestLSP_ParseHover(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"markup content", `{"contents":{"kind":"markdown","value":"func Foo()"}}`, "func Foo()"},
		{"plain string", `{"contents":"just text"}`, "just text"},
		{"marked string array", `{"contents":[{"language":"go","value":"a"},"b"]}`, "a\nb"},
		{"null", `null`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseHover(json.RawMessage(c.raw)); got != c.want {
				t.Fatalf("parseHover = %q, want %q", got, c.want)
			}
		})
	}
}

func TestLSP_ParseSymbols_Hierarchical(t *testing.T) {
	raw := `[{"name":"Foo","kind":12,"range":{"start":{"line":2,"character":0},"end":{"line":5,"character":1}},"selectionRange":{"start":{"line":2,"character":5},"end":{"line":2,"character":8}},"children":[{"name":"bar","kind":13,"range":{"start":{"line":3,"character":1},"end":{"line":3,"character":9}},"selectionRange":{"start":{"line":3,"character":1},"end":{"line":3,"character":4}}}]}]`
	got := parseSymbols(json.RawMessage(raw), "/work", "/work/main.go")
	if len(got) != 2 {
		t.Fatalf("parseSymbols len = %d, want 2 (parent + child)", len(got))
	}
	if got[0].Name != "Foo" || got[0].Kind != "function" {
		t.Errorf("symbol[0] = %+v, want Foo/function", got[0])
	}
	if got[0].Line != 3 || got[0].Column != 6 {
		t.Errorf("symbol[0] position = %d:%d, want 3:6", got[0].Line, got[0].Column)
	}
	if got[1].Name != "bar" || got[1].Container != "Foo" {
		t.Errorf("symbol[1] = %+v, want bar contained in Foo", got[1])
	}
}

func TestLSP_ParseSymbols_FlatSymbolInformation(t *testing.T) {
	raw := `[{"name":"Bar","kind":23,"location":{"uri":"file:///work/x.go","range":{"start":{"line":7,"character":0},"end":{"line":9,"character":1}}},"containerName":"pkg"}]`
	got := parseSymbols(json.RawMessage(raw), "/work", "")
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].File != "x.go" || got[0].Kind != "struct" || got[0].Container != "pkg" {
		t.Errorf("symbol = %+v, want x.go/struct/pkg", got[0])
	}
}

func TestLSP_MarshalResult_Truncates(t *testing.T) {
	res := lspQueryResult{Operation: "references"}
	for i := 0; i < 200; i++ {
		res.Locations = append(res.Locations, lspLocation{File: "some/long/path/file.go", Line: i, Column: 1})
	}
	res.Total = len(res.Locations)
	res.Returned = res.Total
	body, err := marshalLSPResult(res, 1024)
	if err != nil {
		t.Fatalf("marshalLSPResult: %v", err)
	}
	if len(body) > 1024 {
		t.Fatalf("envelope %d bytes, want <= 1024", len(body))
	}
	var got lspQueryResult
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Truncated {
		t.Error("Truncated = false, want true")
	}
	if got.Returned >= 200 {
		t.Errorf("Returned = %d, want < 200 after truncation", got.Returned)
	}
}

// --- helpers ---------------------------------------------------------------

func TestLSP_PathURIRoundTrip(t *testing.T) {
	for _, p := range []string{"/work/main.go", "/var/lib/nomaddev/work/a b.go"} {
		if got := uriToPath(pathToURI(p)); got != p {
			t.Errorf("round trip %q -> %q", p, got)
		}
	}
}

func TestLSP_LangFromPath(t *testing.T) {
	cases := map[string]string{
		"main.go": "go", "app.ts": "typescript", "app.tsx": "typescript",
		"x.js": "javascript", "s.py": "python", "notes.txt": "",
	}
	for path, want := range cases {
		if got := LangFromPath(path); got != want {
			t.Errorf("LangFromPath(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestLSP_SymbolKindName(t *testing.T) {
	cases := map[int]string{1: "file", 5: "class", 12: "function", 23: "struct", 99: "unknown"}
	for kind, want := range cases {
		if got := symbolKindName(kind); got != want {
			t.Errorf("symbolKindName(%d) = %q, want %q", kind, got, want)
		}
	}
}

func TestLSP_Registry_ResolvePath(t *testing.T) {
	reg := NewLSPRegistry(LSPRegistryConfig{
		Servers:      DefaultLSPServers(),
		WorkspaceDir: "/work",
	})
	defer reg.Close()

	if _, err := reg.ResolvePath("sid", "../etc/passwd"); err == nil {
		t.Error("ResolvePath accepted a '..' escape")
	}
	if _, err := reg.ResolvePath("sid", "/abs/path"); err == nil {
		t.Error("ResolvePath accepted an absolute path")
	}
	got, err := reg.ResolvePath("sid", "pkg/a.go")
	if err != nil {
		t.Fatalf("ResolvePath: %v", err)
	}
	if got != filepath.Join("/work", "pkg/a.go") {
		t.Errorf("ResolvePath = %q, want /work/pkg/a.go", got)
	}
}

// --- integration against the fake stdio language server -------------------

func fakeLSPArgv(t *testing.T) []string {
	t.Helper()
	if strings.ContainsAny(os.Args[0], " \t") {
		t.Skip("test binary path contains whitespace; skipping fake-LSP integration")
	}
	return []string{os.Args[0], "-nomaddev-fake-lsp"}
}

func TestLSPServer_Integration(t *testing.T) {
	argv := fakeLSPArgv(t)
	root := t.TempDir()
	mainGo := filepath.Join(root, "main.go")
	if err := os.WriteFile(mainGo, []byte("package main\n\nfunc Foo() {}\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	srv, err := StartLSPServer("go", argv, root, 10*time.Second)
	if err != nil {
		t.Fatalf("StartLSPServer: %v", err)
	}
	defer srv.Shutdown()

	ctx := context.Background()

	t.Run("definition", func(t *testing.T) {
		res := runFakeQuery(t, srv, ctx, LSPQuery{
			Operation: "definition", AbsPath: mainGo, Line: 3, Character: 6,
		})
		if len(res.Locations) != 1 {
			t.Fatalf("locations = %d, want 1", len(res.Locations))
		}
		if filepath.Base(res.Locations[0].File) != "def.go" {
			t.Errorf("file = %q, want def.go", res.Locations[0].File)
		}
		if res.Locations[0].Line != 10 {
			t.Errorf("line = %d, want 10 (0-based 9 -> 1-based)", res.Locations[0].Line)
		}
	})

	t.Run("references", func(t *testing.T) {
		res := runFakeQuery(t, srv, ctx, LSPQuery{
			Operation: "references", AbsPath: mainGo, Line: 3, Character: 6,
			IncludeDeclaration: true,
		})
		if res.Total != 2 || len(res.Locations) != 2 {
			t.Fatalf("references = %d (total %d), want 2", len(res.Locations), res.Total)
		}
	})

	t.Run("hover", func(t *testing.T) {
		res := runFakeQuery(t, srv, ctx, LSPQuery{
			Operation: "hover", AbsPath: mainGo, Line: 3, Character: 6,
		})
		if res.Hover != "func Foo()" {
			t.Errorf("hover = %q, want func Foo()", res.Hover)
		}
	})

	t.Run("document_symbols", func(t *testing.T) {
		res := runFakeQuery(t, srv, ctx, LSPQuery{
			Operation: "document_symbols", AbsPath: mainGo,
		})
		if len(res.Symbols) != 2 {
			t.Fatalf("symbols = %d, want 2", len(res.Symbols))
		}
	})

	t.Run("workspace_symbols", func(t *testing.T) {
		res := runFakeQuery(t, srv, ctx, LSPQuery{
			Operation: "workspace_symbols", Symbol: "Foo",
		})
		if len(res.Symbols) != 1 || res.Symbols[0].Name != "Foo" {
			t.Fatalf("workspace symbols = %+v, want one Foo", res.Symbols)
		}
	})
}

func runFakeQuery(t *testing.T, srv *LSPServer, ctx context.Context, q LSPQuery) lspQueryResult {
	t.Helper()
	body, err := srv.Query(ctx, q, 0)
	if err != nil {
		t.Fatalf("Query(%s): %v", q.Operation, err)
	}
	var res lspQueryResult
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if res.Error != "" {
		t.Fatalf("envelope error: %s", res.Error)
	}
	return res
}

func TestLSPRegistry_GetOrStart_ReusesAndTearsDown(t *testing.T) {
	argv := fakeLSPArgv(t)
	root := t.TempDir()
	reg := NewLSPRegistry(LSPRegistryConfig{
		Servers:      map[string]string{"go": strings.Join(argv, " ")},
		WorkspaceDir: root,
		InitTimeout:  10 * time.Second,
	})
	defer reg.Close()

	first, err := reg.GetOrStart("sid-1", "go")
	if err != nil {
		t.Fatalf("GetOrStart: %v", err)
	}
	second, err := reg.GetOrStart("sid-1", "go")
	if err != nil {
		t.Fatalf("GetOrStart (reuse): %v", err)
	}
	if first != second {
		t.Error("GetOrStart started a second server instead of reusing the warm one")
	}

	if _, err := reg.GetOrStart("sid-1", "ruby"); err == nil {
		t.Error("GetOrStart accepted an unconfigured language")
	}

	if n := reg.StopAllForSession("sid-1"); n != 1 {
		t.Errorf("StopAllForSession stopped %d, want 1", n)
	}
	if !first.isClosed() {
		t.Error("server still open after StopAllForSession")
	}
}
