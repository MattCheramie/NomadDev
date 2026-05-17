package wsserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mattcheramie/nomaddev/internal/event"
	"github.com/mattcheramie/nomaddev/internal/fsops"
	"github.com/mattcheramie/nomaddev/internal/history"
	"github.com/mattcheramie/nomaddev/internal/middleware"
	"github.com/mattcheramie/nomaddev/internal/sandbox"
)

// buildMW assembles a middleware.Service with a mock translator + memory
// history + composite dispatcher (mock sandbox runner + tmp-dir fsops).
type mwOpts struct {
	Translator        *middleware.MockTranslator
	Runner            sandbox.Runner
	WorkspaceDir      string
	AutoGrant         bool
	GateDirectCommand bool
	RequiredTools     []string
	IntentMaxConc     int
	Approver          middleware.Approver // optional override
}

func buildMW(t *testing.T, opts mwOpts) *middleware.Service {
	t.Helper()
	dir := opts.WorkspaceDir
	if dir == "" {
		dir = t.TempDir()
	}
	fsEngine, err := fsops.New(dir)
	if err != nil {
		t.Fatalf("fsops.New: %v", err)
	}
	var approver middleware.Approver
	if opts.Approver != nil {
		approver = opts.Approver
	} else {
		required := opts.RequiredTools
		if required == nil {
			required = []string{middleware.ToolExecuteScript, middleware.ToolWritePatch}
		}
		approver = middleware.NewPolicyApprover(required, opts.AutoGrant, 500*time.Millisecond)
	}
	return &middleware.Service{
		Translator: opts.Translator,
		Dispatcher: middleware.NewCompositeDispatcher(opts.Runner, fsEngine),
		Approver:   approver,
		History:    history.NewMemoryStore(),
		Config: middleware.RuntimeConfig{
			MaxConcurrent:      opts.IntentMaxConc,
			DefaultTimeout:     2 * time.Second,
			GateDirectCommands: opts.GateDirectCommand,
			WindowTurns:        10,
		},
	}
}

func TestMiddleware_NoServiceConfigured_ReturnsNotImplemented(t *testing.T) {
	ts, _, _, issuer := newTestServer(t)
	tok, _ := issuer.Sign("matt", "sess-mw0", nil)
	c, _, err := dialWithAuthHeader(t, ts, tok)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = readEnv(t, c) // hello

	intent, _ := event.NewEnvelope(event.EventUserIntent, event.UserIntentPayload{Text: "hi"})
	writeEnv(t, c, intent)
	got := readEnv(t, c)
	if got.Type != event.EventError {
		t.Fatalf("got %q want error", got.Type)
	}
	var p event.ErrorPayload
	_ = got.UnmarshalPayload(&p)
	if p.Code != event.CodeNotImplemented {
		t.Fatalf("error.code = %q want %q", p.Code, event.CodeNotImplemented)
	}
}

func TestMiddleware_UserIntent_TextOnlyRoundTrip(t *testing.T) {
	mock := middleware.NewMockTranslator([]middleware.AssistantEvent{
		{Text: "hello"},
		{FinalMessage: &middleware.FinalMessage{Text: "hello", FinishReason: "stop"}},
	})
	mw := buildMW(t, mwOpts{Translator: mock, AutoGrant: true})
	ts, _, _, issuer := newTestServerFull(t, testOpts{Middleware: mw})
	tok, _ := issuer.Sign("matt", "sess-text", nil)
	c, _, _ := dialWithAuthHeader(t, ts, tok)
	defer c.Close()
	_ = readEnv(t, c) // hello

	intent, _ := event.NewEnvelope(event.EventUserIntent, event.UserIntentPayload{Text: "hi"})
	writeEnv(t, c, intent)

	var (
		chunk   *event.AssistantChunkPayload
		message *event.AssistantMessagePayload
	)
	for i := 0; i < 5 && message == nil; i++ {
		env := readEnv(t, c)
		if env.CorrelationID != intent.ID {
			t.Fatalf("unexpected correlation %q on %q", env.CorrelationID, env.Type)
		}
		switch env.Type {
		case event.EventAssistantChunk:
			var p event.AssistantChunkPayload
			_ = env.UnmarshalPayload(&p)
			chunk = &p
		case event.EventAssistantMessage:
			var p event.AssistantMessagePayload
			_ = env.UnmarshalPayload(&p)
			message = &p
		default:
			t.Fatalf("unexpected env type %q", env.Type)
		}
	}
	if chunk == nil || chunk.Text != "hello" {
		t.Errorf("chunk = %+v", chunk)
	}
	if message == nil || message.Text != "hello" || message.FinishReason != "stop" {
		t.Errorf("message = %+v", message)
	}
}

func TestMiddleware_UserIntent_FSOpsAutoApproved(t *testing.T) {
	workspace := t.TempDir()
	_ = writeFileSimple(t, workspace, "hello.txt", "from fsops")

	mock := middleware.NewMockTranslator(
		[]middleware.AssistantEvent{
			{ToolCall: &middleware.ToolCall{
				ID: "c1", Tool: middleware.ToolReadFile,
				Args: map[string]any{"path": "hello.txt"},
			}},
		},
		[]middleware.AssistantEvent{
			{FinalMessage: &middleware.FinalMessage{Text: "ok", FinishReason: "stop"}},
		},
	)
	mw := buildMW(t, mwOpts{
		Translator:   mock,
		WorkspaceDir: workspace,
		// read_file is not in the required-tools set, so no approval round-trip
		// should fire even without AutoGrant.
		RequiredTools: []string{middleware.ToolExecuteScript, middleware.ToolWritePatch},
	})
	ts, _, _, issuer := newTestServerFull(t, testOpts{Middleware: mw})
	tok, _ := issuer.Sign("matt", "sess-fs", nil)
	c, _, _ := dialWithAuthHeader(t, ts, tok)
	defer c.Close()
	_ = readEnv(t, c)

	intent, _ := event.NewEnvelope(event.EventUserIntent, event.UserIntentPayload{Text: "read"})
	writeEnv(t, c, intent)

	var sawCmdRequest, sawCmdChunk, sawCmdResult, sawAssistantMsg bool
	for i := 0; i < 10 && !sawAssistantMsg; i++ {
		env := readEnv(t, c)
		switch env.Type {
		case event.EventCommandRequest:
			sawCmdRequest = true
		case event.EventCommandChunk:
			var p event.CommandChunkPayload
			_ = env.UnmarshalPayload(&p)
			if p.Stream == event.StreamStdout && strings.Contains(p.Data, "from fsops") {
				sawCmdChunk = true
			}
		case event.EventCommandResult:
			var p event.CommandResultPayload
			_ = env.UnmarshalPayload(&p)
			if p.ExitCode == 0 {
				sawCmdResult = true
			}
		case event.EventAssistantMessage:
			sawAssistantMsg = true
		case event.EventToolApprovalRequest:
			t.Fatalf("read_file should NOT request approval")
		}
	}
	if !sawCmdRequest || !sawCmdChunk || !sawCmdResult || !sawAssistantMsg {
		t.Fatalf("missing leg: req=%v chunk=%v result=%v msg=%v", sawCmdRequest, sawCmdChunk, sawCmdResult, sawAssistantMsg)
	}
}

func TestMiddleware_UserIntent_ToolCallApprovalGranted(t *testing.T) {
	runner := sandbox.NewMockRunner(sandbox.MockScript("ran!\n", "", 0)...)
	mock := middleware.NewMockTranslator(
		[]middleware.AssistantEvent{
			{ToolCall: &middleware.ToolCall{
				ID: "c1", Tool: middleware.ToolExecuteScript,
				Args: map[string]any{"script": "echo ran"},
			}},
		},
		[]middleware.AssistantEvent{
			{FinalMessage: &middleware.FinalMessage{Text: "done", FinishReason: "stop"}},
		},
	)
	mw := buildMW(t, mwOpts{Translator: mock, Runner: runner})
	ts, _, _, issuer := newTestServerFull(t, testOpts{Middleware: mw})
	tok, _ := issuer.Sign("matt", "sess-app", nil)
	c, _, _ := dialWithAuthHeader(t, ts, tok)
	defer c.Close()
	_ = readEnv(t, c)

	intent, _ := event.NewEnvelope(event.EventUserIntent, event.UserIntentPayload{Text: "run echo"})
	writeEnv(t, c, intent)

	// Expect: command.request, tool.approval.request, then we send grant,
	// then chunk, result, final assistant.message.
	var approvalReqID, cmdReqID string
	var sawResult, sawMsg bool
	for i := 0; i < 12 && !sawMsg; i++ {
		env := readEnv(t, c)
		switch env.Type {
		case event.EventCommandRequest:
			cmdReqID = env.ID
		case event.EventToolApprovalRequest:
			approvalReqID = env.ID
			// Send the grant.
			g, _ := event.NewReply(event.EventToolApprovalGranted, approvalReqID, event.ToolApprovalGrantedPayload{})
			writeEnv(t, c, g)
		case event.EventCommandChunk:
			if env.CorrelationID != cmdReqID {
				t.Errorf("chunk correlation = %q, want %q", env.CorrelationID, cmdReqID)
			}
		case event.EventCommandResult:
			var p event.CommandResultPayload
			_ = env.UnmarshalPayload(&p)
			if p.ExitCode != 0 || p.Error != "" {
				t.Errorf("command.result = %+v", p)
			}
			sawResult = true
		case event.EventAssistantMessage:
			sawMsg = true
		}
	}
	if !sawResult || !sawMsg {
		t.Fatalf("flow incomplete: result=%v msg=%v", sawResult, sawMsg)
	}
}

func TestMiddleware_UserIntent_ToolCallApprovalDenied(t *testing.T) {
	runner := sandbox.NewMockRunner(sandbox.MockScript("never\n", "", 0)...)
	mock := middleware.NewMockTranslator(
		[]middleware.AssistantEvent{
			{ToolCall: &middleware.ToolCall{
				ID: "c1", Tool: middleware.ToolExecuteScript,
				Args: map[string]any{"script": "echo nope"},
			}},
		},
		[]middleware.AssistantEvent{
			{FinalMessage: &middleware.FinalMessage{Text: "ok", FinishReason: "stop"}},
		},
	)
	mw := buildMW(t, mwOpts{Translator: mock, Runner: runner})
	ts, _, _, issuer := newTestServerFull(t, testOpts{Middleware: mw})
	tok, _ := issuer.Sign("matt", "sess-deny", nil)
	c, _, _ := dialWithAuthHeader(t, ts, tok)
	defer c.Close()
	_ = readEnv(t, c)

	intent, _ := event.NewEnvelope(event.EventUserIntent, event.UserIntentPayload{Text: "no"})
	writeEnv(t, c, intent)

	var result *event.CommandResultPayload
	var sawMsg bool
	for i := 0; i < 12 && !sawMsg; i++ {
		env := readEnv(t, c)
		switch env.Type {
		case event.EventToolApprovalRequest:
			d, _ := event.NewReply(event.EventToolApprovalDenied, env.ID, event.ToolApprovalDeniedPayload{Reason: "no"})
			writeEnv(t, c, d)
		case event.EventCommandChunk:
			t.Fatal("denied call should not stream chunks")
		case event.EventCommandResult:
			var p event.CommandResultPayload
			_ = env.UnmarshalPayload(&p)
			result = &p
		case event.EventAssistantMessage:
			sawMsg = true
		}
	}
	if result == nil || result.Error != event.SandboxErrUnauthorized {
		t.Fatalf("result = %+v", result)
	}
	if runner.ExecCalls() != 0 {
		t.Errorf("runner.Exec called %d times; want 0 on denial", runner.ExecCalls())
	}
}

func TestMiddleware_UserIntent_ApprovalTimeout(t *testing.T) {
	runner := sandbox.NewMockRunner(sandbox.MockScript("never\n", "", 0)...)
	mock := middleware.NewMockTranslator(
		[]middleware.AssistantEvent{
			{ToolCall: &middleware.ToolCall{
				ID: "c1", Tool: middleware.ToolExecuteScript,
				Args: map[string]any{"script": "echo nope"},
			}},
		},
		[]middleware.AssistantEvent{
			{FinalMessage: &middleware.FinalMessage{Text: "ok", FinishReason: "stop"}},
		},
	)
	// Tight approval timeout.
	app := middleware.NewPolicyApprover(
		[]string{middleware.ToolExecuteScript, middleware.ToolWritePatch}, false, 50*time.Millisecond)
	mw := buildMW(t, mwOpts{Translator: mock, Runner: runner, Approver: app})
	ts, _, _, issuer := newTestServerFull(t, testOpts{Middleware: mw})
	tok, _ := issuer.Sign("matt", "sess-to", nil)
	c, _, _ := dialWithAuthHeader(t, ts, tok)
	defer c.Close()
	_ = readEnv(t, c)

	intent, _ := event.NewEnvelope(event.EventUserIntent, event.UserIntentPayload{Text: "x"})
	writeEnv(t, c, intent)

	var result *event.CommandResultPayload
	var sawMsg bool
	for i := 0; i < 12 && !sawMsg; i++ {
		env := readEnv(t, c)
		switch env.Type {
		case event.EventToolApprovalRequest:
			// Deliberately do NOT reply.
		case event.EventCommandResult:
			var p event.CommandResultPayload
			_ = env.UnmarshalPayload(&p)
			result = &p
		case event.EventAssistantMessage:
			sawMsg = true
		}
	}
	if result == nil || result.Error != event.SandboxErrUnauthorized {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.ErrorMessage, "timed out") {
		t.Errorf("ErrorMessage = %q", result.ErrorMessage)
	}
}

func TestMiddleware_UserIntent_HistoryAppended(t *testing.T) {
	mock := middleware.NewMockTranslator([]middleware.AssistantEvent{
		{Text: "hi"},
		{FinalMessage: &middleware.FinalMessage{Text: "hi", FinishReason: "stop"}},
	})
	mw := buildMW(t, mwOpts{Translator: mock, AutoGrant: true})
	ts, _, _, issuer := newTestServerFull(t, testOpts{Middleware: mw})
	tok, _ := issuer.Sign("matt", "sess-hist", nil)
	c, _, _ := dialWithAuthHeader(t, ts, tok)
	defer c.Close()
	_ = readEnv(t, c)

	intent, _ := event.NewEnvelope(event.EventUserIntent, event.UserIntentPayload{Text: "ping"})
	writeEnv(t, c, intent)
	for i := 0; i < 4; i++ {
		env := readEnv(t, c)
		if env.Type == event.EventAssistantMessage {
			break
		}
	}
	rows, err := mw.History.LoadWindow(context.Background(), "sess-hist", 0)
	if err != nil {
		t.Fatalf("LoadWindow: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 turns (user + assistant), got %d", len(rows))
	}
	if rows[0].Role != history.RoleUser || rows[1].Role != history.RoleAssistant {
		t.Errorf("roles = %v / %v", rows[0].Role, rows[1].Role)
	}
}

func TestMiddleware_DirectCommandRequest_GatedByPolicy(t *testing.T) {
	runner := sandbox.NewMockRunner(sandbox.MockScript("never\n", "", 0)...)
	mock := middleware.NewMockTranslator([]middleware.AssistantEvent{{FinalMessage: &middleware.FinalMessage{Text: "", FinishReason: "stop"}}})
	mw := buildMW(t, mwOpts{
		Translator:        mock,
		Runner:            runner,
		GateDirectCommand: true,
	})
	ts, _, _, issuer := newTestServerFull(t, testOpts{Middleware: mw, Runner: runner})
	tok, _ := issuer.Sign("matt", "sess-direct", nil)
	c, _, _ := dialWithAuthHeader(t, ts, tok)
	defer c.Close()
	_ = readEnv(t, c)

	req, _ := event.NewEnvelope(event.EventCommandRequest, event.CommandRequestPayload{
		Tool: middleware.ToolExecuteScript,
		Args: map[string]any{"script": "echo hi"},
	})
	writeEnv(t, c, req)

	got := readEnv(t, c)
	if got.Type != event.EventToolApprovalRequest {
		t.Fatalf("got %q want tool.approval.request", got.Type)
	}
}

func TestMiddleware_DirectCommandRequest_BypassWhenAutoGrant(t *testing.T) {
	runner := sandbox.NewMockRunner(sandbox.MockScript("yes\n", "", 0)...)
	mock := middleware.NewMockTranslator([]middleware.AssistantEvent{{FinalMessage: &middleware.FinalMessage{Text: "", FinishReason: "stop"}}})
	mw := buildMW(t, mwOpts{
		Translator:        mock,
		Runner:            runner,
		GateDirectCommand: true,
		AutoGrant:         true,
	})
	ts, _, _, issuer := newTestServerFull(t, testOpts{Middleware: mw, Runner: runner})
	tok, _ := issuer.Sign("matt", "sess-direct-auto", nil)
	c, _, _ := dialWithAuthHeader(t, ts, tok)
	defer c.Close()
	_ = readEnv(t, c)

	req, _ := event.NewEnvelope(event.EventCommandRequest, event.CommandRequestPayload{
		Tool: middleware.ToolExecuteScript,
		Args: map[string]any{"script": "echo hi"},
	})
	writeEnv(t, c, req)

	// First frame should be a command.chunk (or command.result if the mock is
	// fast enough). Definitely NOT an approval.request.
	got := readEnv(t, c)
	if got.Type == event.EventToolApprovalRequest {
		t.Fatalf("auto-grant should bypass approval; got %q", got.Type)
	}
}

func TestMiddleware_ConcurrencyLimit_Intents(t *testing.T) {
	mock := middleware.NewMockTranslator(
		[]middleware.AssistantEvent{
			{Text: "slow"},
			{FinalMessage: &middleware.FinalMessage{Text: "done", FinishReason: "stop"}},
		},
	)
	mock.PerEventDelay = 200 * time.Millisecond

	mw := buildMW(t, mwOpts{Translator: mock, IntentMaxConc: 1, AutoGrant: true})
	ts, _, _, issuer := newTestServerFull(t, testOpts{Middleware: mw})
	tok, _ := issuer.Sign("matt", "sess-cap", nil)
	c, _, _ := dialWithAuthHeader(t, ts, tok)
	defer c.Close()
	_ = readEnv(t, c)

	first, _ := event.NewEnvelope(event.EventUserIntent, event.UserIntentPayload{Text: "1"})
	writeEnv(t, c, first)
	second, _ := event.NewEnvelope(event.EventUserIntent, event.UserIntentPayload{Text: "2"})
	writeEnv(t, c, second)

	// Scan for the at-capacity assistant.message tied to the second intent.
	found := false
	for i := 0; i < 10 && !found; i++ {
		env := readEnv(t, c)
		if env.Type == event.EventAssistantMessage && env.CorrelationID == second.ID {
			var p event.AssistantMessagePayload
			_ = env.UnmarshalPayload(&p)
			if p.FinishReason == "error" && strings.Contains(p.Error, "capacity") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("expected at-capacity error on second intent")
	}
}

// writeFileSimple is a small helper that the test reuses for the fsops setup.
func writeFileSimple(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write %q: %v", p, err)
	}
	return p
}
