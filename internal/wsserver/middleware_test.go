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
	MaxAutoRetries    int                 // 0 = recovery loop disabled
	// Provider / Model populate RuntimeConfig so the set_model handler and
	// hello-payload tests can exercise the model-switch surface without
	// pulling in a real translator backend.
	Provider string
	Model    string
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
			Provider:           opts.Provider,
			Model:              opts.Model,
			MaxConcurrent:      opts.IntentMaxConc,
			DefaultTimeout:     2 * time.Second,
			GateDirectCommands: opts.GateDirectCommand,
			WindowTurns:        10,
			MaxAutoRetries:     opts.MaxAutoRetries,
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

func TestMiddleware_UserIntent_ClientDisconnectDuringApproval(t *testing.T) {
	runner := sandbox.NewMockRunner(sandbox.MockScript("never\n", "", 0)...)
	mock := middleware.NewMockTranslator(
		[]middleware.AssistantEvent{
			{ToolCall: &middleware.ToolCall{
				ID: "c1", Tool: middleware.ToolExecuteScript,
				Args: map[string]any{"script": "echo nope"},
			}},
		},
		[]middleware.AssistantEvent{
			{FinalMessage: &middleware.FinalMessage{Text: "should not reach", FinishReason: "stop"}},
		},
	)
	// Long approval timeout — the test ends the wait by closing the connection,
	// not by timing out.
	app := middleware.NewPolicyApprover(
		[]string{middleware.ToolExecuteScript, middleware.ToolWritePatch}, false, 30*time.Second)
	mw := buildMW(t, mwOpts{Translator: mock, Runner: runner, Approver: app})
	ts, _, _, issuer := newTestServerFull(t, testOpts{Middleware: mw})
	tok, _ := issuer.Sign("matt", "sess-disc", nil)
	c, _, _ := dialWithAuthHeader(t, ts, tok)
	_ = readEnv(t, c) // hello

	intent, _ := event.NewEnvelope(event.EventUserIntent, event.UserIntentPayload{Text: "x"})
	writeEnv(t, c, intent)

	// Read until we see the approval request, then drop the connection.
	for i := 0; i < 5; i++ {
		env := readEnv(t, c)
		if env.Type == event.EventToolApprovalRequest {
			break
		}
		if i == 4 {
			t.Fatal("never saw tool.approval.request")
		}
	}
	_ = c.Close()

	// Give the handler a beat to observe the disconnect and unwind.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if mock.Streams() == 1 && runner.ExecCalls() == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if mock.Streams() != 1 {
		t.Errorf("translator stream count = %d; want 1 (resume must not fire after disconnect)", mock.Streams())
	}
	if runner.ExecCalls() != 0 {
		t.Errorf("runner.Exec called %d times; want 0 (dispatch must not fire after disconnect)", runner.ExecCalls())
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

// TestMiddleware_AutoRetry_SingleFailureRecovers verifies that when a tool
// call fails once and the second attempt succeeds, the orchestrator:
//   - feeds a system.error_report into the translator via ResumeFunc
//   - does NOT emit a system.error_report envelope on the wire
//   - terminates the turn cleanly with FinishReason=stop
func TestMiddleware_AutoRetry_SingleFailureRecovers(t *testing.T) {
	// First Exec call fails with exit 1; second succeeds.
	runner := &flakeyRunner{scripts: [][]sandbox.ExecChunk{
		sandbox.MockScript("", "boom\n", 1),
		sandbox.MockScript("ok\n", "", 0),
	}}
	mock := middleware.NewMockTranslator(
		[]middleware.AssistantEvent{
			{ToolCall: &middleware.ToolCall{
				ID: "c1", Tool: middleware.ToolExecuteScript,
				Args: map[string]any{"script": "false"},
			}},
		},
		[]middleware.AssistantEvent{
			{ToolCall: &middleware.ToolCall{
				ID: "c2", Tool: middleware.ToolExecuteScript,
				Args: map[string]any{"script": "true"},
			}},
		},
		[]middleware.AssistantEvent{
			{FinalMessage: &middleware.FinalMessage{Text: "fixed", FinishReason: "stop"}},
		},
	)
	mw := buildMW(t, mwOpts{
		Translator: mock, Runner: runner, AutoGrant: true, MaxAutoRetries: 2,
	})
	ts, _, _, issuer := newTestServerFull(t, testOpts{Middleware: mw})
	tok, _ := issuer.Sign("matt", "sess-retry-ok", nil)
	c, _, _ := dialWithAuthHeader(t, ts, tok)
	defer c.Close()
	_ = readEnv(t, c)

	intent, _ := event.NewEnvelope(event.EventUserIntent, event.UserIntentPayload{Text: "fix me"})
	writeEnv(t, c, intent)

	var sawErrorReportEnv bool
	var finishReason string
	cmdRequests := 0
	for i := 0; i < 20; i++ {
		env := readEnv(t, c)
		switch env.Type {
		case event.EventCommandRequest:
			cmdRequests++
		case event.EventSystemErrorReport:
			sawErrorReportEnv = true
		case event.EventAssistantMessage:
			var p event.AssistantMessagePayload
			_ = env.UnmarshalPayload(&p)
			finishReason = p.FinishReason
		}
		if finishReason != "" {
			break
		}
	}
	if sawErrorReportEnv {
		t.Errorf("must not emit system.error_report envelope when retry succeeds")
	}
	if finishReason != "stop" {
		t.Errorf("finish_reason = %q, want \"stop\"", finishReason)
	}
	if cmdRequests != 2 {
		t.Errorf("command.request count = %d, want 2 (initial + 1 retry)", cmdRequests)
	}
	if runner.calls != 2 {
		t.Errorf("runner.Exec calls = %d, want 2", runner.calls)
	}
	// The first ResumedResult must carry the error_report enrichment.
	resumes := mock.ResumedResults()
	if len(resumes) < 1 {
		t.Fatalf("ResumedResults = %d, want >=1", len(resumes))
	}
	report, ok := resumes[0].Output[middleware.ToolResultErrorReportKey].(event.SystemErrorReportPayload)
	if !ok {
		t.Fatalf("first resume output[%q] type = %T, want SystemErrorReportPayload",
			middleware.ToolResultErrorReportKey, resumes[0].Output[middleware.ToolResultErrorReportKey])
	}
	if report.Tool != middleware.ToolExecuteScript || report.ExitCode != 1 || report.Escalated {
		t.Errorf("error_report = %+v", report)
	}
	if report.Attempt != 1 || report.MaxAttempts != 3 {
		t.Errorf("attempt/max = %d/%d, want 1/3", report.Attempt, report.MaxAttempts)
	}
	if !strings.Contains(report.Stderr, "boom") {
		t.Errorf("Stderr = %q, want it to contain \"boom\"", report.Stderr)
	}
}

// TestMiddleware_AutoRetry_BudgetExhaustedEscalates verifies that when every
// retry fails, the orchestrator emits a system.error_report envelope on the
// wire (the Mobile Control Hub escalation) and closes the turn with
// FinishReason=error.
func TestMiddleware_AutoRetry_BudgetExhaustedEscalates(t *testing.T) {
	// Every Exec call fails with exit 7.
	runner := &flakeyRunner{always: sandbox.MockScript("", "still broken\n", 7)}
	failingCall := middleware.AssistantEvent{ToolCall: &middleware.ToolCall{
		ID: "cN", Tool: middleware.ToolExecuteScript,
		Args: map[string]any{"script": "exit 7"},
	}}
	mock := middleware.NewMockTranslator(
		[]middleware.AssistantEvent{failingCall},
		[]middleware.AssistantEvent{failingCall},
		[]middleware.AssistantEvent{failingCall},
		[]middleware.AssistantEvent{
			// Never reached — orchestrator should terminate before this.
			{FinalMessage: &middleware.FinalMessage{Text: "won't run", FinishReason: "stop"}},
		},
	)
	mw := buildMW(t, mwOpts{
		Translator: mock, Runner: runner, AutoGrant: true, MaxAutoRetries: 2,
	})
	ts, _, _, issuer := newTestServerFull(t, testOpts{Middleware: mw})
	tok, _ := issuer.Sign("matt", "sess-retry-bust", nil)
	c, _, _ := dialWithAuthHeader(t, ts, tok)
	defer c.Close()
	_ = readEnv(t, c)

	intent, _ := event.NewEnvelope(event.EventUserIntent, event.UserIntentPayload{Text: "doomed"})
	writeEnv(t, c, intent)

	var escalation *event.SystemErrorReportPayload
	cmdRequests := 0
	var finishReason string
	for i := 0; i < 30; i++ {
		env := readEnv(t, c)
		switch env.Type {
		case event.EventCommandRequest:
			cmdRequests++
		case event.EventSystemErrorReport:
			var p event.SystemErrorReportPayload
			_ = env.UnmarshalPayload(&p)
			escalation = &p
		case event.EventAssistantMessage:
			var p event.AssistantMessagePayload
			_ = env.UnmarshalPayload(&p)
			finishReason = p.FinishReason
		}
		if finishReason != "" {
			break
		}
	}
	if escalation == nil {
		t.Fatalf("expected one system.error_report envelope; got none")
	}
	if !escalation.Escalated {
		t.Errorf("system.error_report.Escalated = false; want true")
	}
	if escalation.Attempt != 3 || escalation.MaxAttempts != 3 {
		t.Errorf("attempt/max = %d/%d, want 3/3", escalation.Attempt, escalation.MaxAttempts)
	}
	if escalation.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", escalation.ExitCode)
	}
	if !strings.Contains(escalation.Stderr, "still broken") {
		t.Errorf("Stderr = %q, want \"still broken\"", escalation.Stderr)
	}
	if finishReason != "error" {
		t.Errorf("finish_reason = %q, want \"error\"", finishReason)
	}
	if cmdRequests != 3 {
		t.Errorf("command.request count = %d, want 3 (initial + 2 retries)", cmdRequests)
	}
}

// TestMiddleware_AutoRetry_ZeroBudgetEscalatesImmediately verifies that with
// MaxAutoRetries=0 the first failure escalates without trying to feed the
// error back through the translator.
func TestMiddleware_AutoRetry_ZeroBudgetEscalatesImmediately(t *testing.T) {
	runner := &flakeyRunner{always: sandbox.MockScript("", "nope\n", 2)}
	failingCall := middleware.AssistantEvent{ToolCall: &middleware.ToolCall{
		ID: "c0", Tool: middleware.ToolExecuteScript,
		Args: map[string]any{"script": "exit 2"},
	}}
	mock := middleware.NewMockTranslator(
		[]middleware.AssistantEvent{failingCall},
		[]middleware.AssistantEvent{
			{FinalMessage: &middleware.FinalMessage{Text: "x", FinishReason: "stop"}},
		},
	)
	mw := buildMW(t, mwOpts{
		Translator: mock, Runner: runner, AutoGrant: true, MaxAutoRetries: 0,
	})
	ts, _, _, issuer := newTestServerFull(t, testOpts{Middleware: mw})
	tok, _ := issuer.Sign("matt", "sess-retry-zero", nil)
	c, _, _ := dialWithAuthHeader(t, ts, tok)
	defer c.Close()
	_ = readEnv(t, c)

	intent, _ := event.NewEnvelope(event.EventUserIntent, event.UserIntentPayload{Text: "boom"})
	writeEnv(t, c, intent)

	var escalation *event.SystemErrorReportPayload
	cmdRequests := 0
	var finishReason string
	for i := 0; i < 12; i++ {
		env := readEnv(t, c)
		switch env.Type {
		case event.EventCommandRequest:
			cmdRequests++
		case event.EventSystemErrorReport:
			var p event.SystemErrorReportPayload
			_ = env.UnmarshalPayload(&p)
			escalation = &p
		case event.EventAssistantMessage:
			var p event.AssistantMessagePayload
			_ = env.UnmarshalPayload(&p)
			finishReason = p.FinishReason
		}
		if finishReason != "" {
			break
		}
	}
	if escalation == nil || !escalation.Escalated {
		t.Fatalf("expected escalation envelope; got %+v", escalation)
	}
	if escalation.Attempt != 1 || escalation.MaxAttempts != 1 {
		t.Errorf("attempt/max = %d/%d, want 1/1", escalation.Attempt, escalation.MaxAttempts)
	}
	if cmdRequests != 1 {
		t.Errorf("command.request count = %d, want 1 (no retries)", cmdRequests)
	}
	if finishReason != "error" {
		t.Errorf("finish_reason = %q, want \"error\"", finishReason)
	}
}

// TestMiddleware_AutoRetry_NonRetryableFailureDoesNotEscalate verifies that
// failures the recovery loop classifies as non-retryable (e.g. bad_request
// from invalid args) flow through the existing path: no error_report
// envelope, no resumed error_report enrichment.
func TestMiddleware_AutoRetry_NonRetryableFailureDoesNotEscalate(t *testing.T) {
	// Invalid args (script field missing) → middleware.Validate rejects
	// before dispatch with SandboxErrBadRequest.
	mock := middleware.NewMockTranslator(
		[]middleware.AssistantEvent{
			{ToolCall: &middleware.ToolCall{
				ID: "cX", Tool: middleware.ToolExecuteScript,
				Args: map[string]any{}, // missing "script"
			}},
		},
		[]middleware.AssistantEvent{
			{FinalMessage: &middleware.FinalMessage{Text: "done", FinishReason: "stop"}},
		},
	)
	runner := sandbox.NewMockRunner(sandbox.MockScript("never\n", "", 0)...)
	mw := buildMW(t, mwOpts{
		Translator: mock, Runner: runner, AutoGrant: true, MaxAutoRetries: 2,
	})
	ts, _, _, issuer := newTestServerFull(t, testOpts{Middleware: mw})
	tok, _ := issuer.Sign("matt", "sess-retry-nr", nil)
	c, _, _ := dialWithAuthHeader(t, ts, tok)
	defer c.Close()
	_ = readEnv(t, c)

	intent, _ := event.NewEnvelope(event.EventUserIntent, event.UserIntentPayload{Text: "bad"})
	writeEnv(t, c, intent)

	var sawErrorReportEnv bool
	var finishReason string
	for i := 0; i < 12; i++ {
		env := readEnv(t, c)
		switch env.Type {
		case event.EventSystemErrorReport:
			sawErrorReportEnv = true
		case event.EventAssistantMessage:
			var p event.AssistantMessagePayload
			_ = env.UnmarshalPayload(&p)
			finishReason = p.FinishReason
		}
		if finishReason != "" {
			break
		}
	}
	if sawErrorReportEnv {
		t.Errorf("non-retryable failure must not emit system.error_report envelope")
	}
	if finishReason != "stop" {
		t.Errorf("finish_reason = %q, want \"stop\"", finishReason)
	}
	resumes := mock.ResumedResults()
	if len(resumes) == 0 {
		t.Fatalf("expected at least one resume call")
	}
	if _, present := resumes[0].Output[middleware.ToolResultErrorReportKey]; present {
		t.Errorf("non-retryable failure must NOT enrich Output with error_report")
	}
}

// flakeyRunner is a test sandbox.Runner that emits a different scripted
// output per Exec call. If `always` is non-nil, every call returns that
// script (used to force a sustained failure).
type flakeyRunner struct {
	scripts [][]sandbox.ExecChunk
	always  []sandbox.ExecChunk
	calls   int
}

func (r *flakeyRunner) Exec(ctx context.Context, _ sandbox.ExecRequest) (<-chan sandbox.ExecChunk, error) {
	idx := r.calls
	r.calls++
	var script []sandbox.ExecChunk
	if r.always != nil {
		script = r.always
	} else if idx < len(r.scripts) {
		script = r.scripts[idx]
	} else {
		script = sandbox.MockScript("", "", 0)
	}
	out := make(chan sandbox.ExecChunk, len(script))
	go func() {
		defer close(out)
		for _, c := range script {
			select {
			case out <- c:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}
