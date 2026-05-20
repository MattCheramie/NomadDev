package wsserver

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/mattcheramie/nomaddev/internal/event"
	"github.com/mattcheramie/nomaddev/internal/middleware"
	"github.com/mattcheramie/nomaddev/internal/sandbox"
)

// listDaemonIDs drives a list_daemons command.request and returns the ids of
// the daemons reported for the connection's session.
func listDaemonIDs(t *testing.T, c *websocket.Conn) []string {
	t.Helper()
	req, _ := event.NewEnvelope(event.EventCommandRequest, event.CommandRequestPayload{
		Tool: sandbox.ToolListDaemons,
	})
	writeEnv(t, c, req)

	var body string
	for i := 0; i < 6; i++ {
		env := readEnv(t, c)
		switch env.Type {
		case event.EventCommandChunk:
			var p event.CommandChunkPayload
			_ = env.UnmarshalPayload(&p)
			if p.Stream == event.StreamStdout {
				body += p.Data
			}
		case event.EventCommandResult:
			var parsed struct {
				Daemons []struct {
					ID string `json:"id"`
				} `json:"daemons"`
			}
			if body != "" {
				if err := json.Unmarshal([]byte(body), &parsed); err != nil {
					t.Fatalf("list_daemons JSON: %v (%q)", err, body)
				}
			}
			ids := make([]string, 0, len(parsed.Daemons))
			for _, d := range parsed.Daemons {
				ids = append(ids, d.ID)
			}
			return ids
		}
	}
	t.Fatal("list_daemons: no command.result observed")
	return nil
}

// readUntilResult reads frames until a command.result and returns it.
func readUntilResult(t *testing.T, c *websocket.Conn) event.CommandResultPayload {
	t.Helper()
	for i := 0; i < 10; i++ {
		env := readEnv(t, c)
		if env.Type == event.EventCommandResult {
			var p event.CommandResultPayload
			_ = env.UnmarshalPayload(&p)
			return p
		}
	}
	t.Fatal("no command.result observed")
	return event.CommandResultPayload{}
}

func TestDaemon_MonitorDaemon_ApprovalAndStreaming(t *testing.T) {
	mw := buildMW(t, mwOpts{RequiredTools: []string{middleware.ToolMonitorDaemon}})
	ts, _, _, issuer := newTestServerFull(t, testOpts{Middleware: mw, DaemonEnabled: true})
	tok, _ := issuer.Sign("matt", "sess-mon", nil)
	c, _, err := dialWithAuthHeader(t, ts, tok)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = readEnv(t, c) // hello

	req, _ := event.NewEnvelope(event.EventCommandRequest, event.CommandRequestPayload{
		Tool: sandbox.ToolMonitorDaemon,
		Args: map[string]any{"command": `printf 'hi\n'`},
	})
	writeEnv(t, c, req)

	var (
		sawApproval bool
		sawResult   bool
		resultErr   string
		logLines    []string
		sawClosed   bool
		closedExit  int
	)
	for i := 0; i < 15 && (!sawResult || !sawClosed); i++ {
		env := readEnv(t, c)
		switch env.Type {
		case event.EventToolApprovalRequest:
			sawApproval = true
			if env.CorrelationID != req.ID {
				t.Errorf("approval correlation = %q, want %q", env.CorrelationID, req.ID)
			}
			g, _ := event.NewReply(event.EventToolApprovalGranted, env.ID, event.ToolApprovalGrantedPayload{})
			writeEnv(t, c, g)
		case event.EventCommandResult:
			var p event.CommandResultPayload
			_ = env.UnmarshalPayload(&p)
			sawResult = true
			resultErr = p.Error
		case event.EventSystemLogEvent:
			var p event.SystemLogEventPayload
			_ = env.UnmarshalPayload(&p)
			if env.CorrelationID != req.ID {
				t.Errorf("log_event correlation = %q, want %q", env.CorrelationID, req.ID)
			}
			if p.DaemonID == "" {
				t.Error("log_event missing daemon_id")
			}
			if p.Closed {
				sawClosed = true
				closedExit = p.ExitCode
			} else if p.Line != "" {
				logLines = append(logLines, p.Line)
			}
		}
	}

	if !sawApproval {
		t.Error("monitor_daemon was not approval-gated")
	}
	if !sawResult || resultErr != "" {
		t.Fatalf("command.result missing or errored: saw=%v err=%q", sawResult, resultErr)
	}
	if len(logLines) != 1 || logLines[0] != "hi" {
		t.Fatalf("log lines = %v, want [hi]", logLines)
	}
	if !sawClosed || closedExit != 0 {
		t.Fatalf("closed frame missing or non-zero exit: saw=%v exit=%d", sawClosed, closedExit)
	}
}

func TestDaemon_KilledOnDisconnect(t *testing.T) {
	mw := buildMW(t, mwOpts{AutoGrant: true})
	ts, srv, _, issuer := newTestServerFull(t, testOpts{Middleware: mw, DaemonEnabled: true})
	tok, _ := issuer.Sign("matt", "sess-dc", nil)
	c, _, err := dialWithAuthHeader(t, ts, tok)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = readEnv(t, c) // hello

	req, _ := event.NewEnvelope(event.EventCommandRequest, event.CommandRequestPayload{
		Tool: sandbox.ToolMonitorDaemon,
		Args: map[string]any{"command": "sleep 30"},
	})
	writeEnv(t, c, req)
	if r := readUntilResult(t, c); r.Error != "" {
		t.Fatalf("monitor_daemon result error = %q", r.Error)
	}
	if got := len(srv.daemons.List("sess-dc")); got != 1 {
		t.Fatalf("registered daemons = %d, want 1 before disconnect", got)
	}

	// Closing the connection must reap every daemon for the session.
	_ = c.Close()
	reaped := false
	for i := 0; i < 100 && !reaped; i++ {
		if len(srv.daemons.List("sess-dc")) == 0 {
			reaped = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !reaped {
		t.Fatal("daemon not reaped after the connection closed")
	}
}

func TestDaemon_ListAndStop(t *testing.T) {
	mw := buildMW(t, mwOpts{AutoGrant: true})
	ts, _, _, issuer := newTestServerFull(t, testOpts{Middleware: mw, DaemonEnabled: true})
	tok, _ := issuer.Sign("matt", "sess-ls", nil)
	c, _, err := dialWithAuthHeader(t, ts, tok)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = readEnv(t, c) // hello

	mon, _ := event.NewEnvelope(event.EventCommandRequest, event.CommandRequestPayload{
		Tool: sandbox.ToolMonitorDaemon,
		Args: map[string]any{"command": "sleep 30"},
	})
	writeEnv(t, c, mon)
	if r := readUntilResult(t, c); r.Error != "" {
		t.Fatalf("monitor_daemon result error = %q", r.Error)
	}

	ids := listDaemonIDs(t, c)
	if len(ids) != 1 {
		t.Fatalf("list_daemons = %v, want one daemon", ids)
	}

	stop, _ := event.NewEnvelope(event.EventCommandRequest, event.CommandRequestPayload{
		Tool: sandbox.ToolStopDaemon,
		Args: map[string]any{"daemon_id": ids[0]},
	})
	writeEnv(t, c, stop)
	if r := readUntilResult(t, c); r.Error != "" {
		t.Fatalf("stop_daemon result error = %q", r.Error)
	}

	if ids := listDaemonIDs(t, c); len(ids) != 0 {
		t.Fatalf("list_daemons after stop = %v, want empty", ids)
	}
}

func TestDaemon_StopUnknownDaemonFails(t *testing.T) {
	mw := buildMW(t, mwOpts{AutoGrant: true})
	ts, _, _, issuer := newTestServerFull(t, testOpts{Middleware: mw, DaemonEnabled: true})
	tok, _ := issuer.Sign("matt", "sess-unk", nil)
	c, _, err := dialWithAuthHeader(t, ts, tok)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = readEnv(t, c) // hello

	stop, _ := event.NewEnvelope(event.EventCommandRequest, event.CommandRequestPayload{
		Tool: sandbox.ToolStopDaemon,
		Args: map[string]any{"daemon_id": "01HXNOSUCHDAEMON"},
	})
	writeEnv(t, c, stop)
	if r := readUntilResult(t, c); r.Error != event.SandboxErrBadRequest {
		t.Fatalf("stop_daemon unknown id: error = %q, want %q", r.Error, event.SandboxErrBadRequest)
	}
}

func TestDaemon_DisabledReturnsBadRequest(t *testing.T) {
	// No DaemonEnabled — the registry is nil and monitor_daemon must fail.
	ts, _, _, issuer := newTestServer(t)
	tok, _ := issuer.Sign("matt", "sess-off", nil)
	c, _, err := dialWithAuthHeader(t, ts, tok)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = readEnv(t, c) // hello

	req, _ := event.NewEnvelope(event.EventCommandRequest, event.CommandRequestPayload{
		Tool: sandbox.ToolMonitorDaemon,
		Args: map[string]any{"command": "echo hi"},
	})
	writeEnv(t, c, req)
	if r := readUntilResult(t, c); r.Error != event.SandboxErrBadRequest {
		t.Fatalf("disabled monitor_daemon: error = %q, want %q", r.Error, event.SandboxErrBadRequest)
	}
}
