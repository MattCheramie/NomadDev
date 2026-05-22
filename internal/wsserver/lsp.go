package wsserver

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/mattcheramie/nomaddev/internal/event"
	"github.com/mattcheramie/nomaddev/internal/hub"
	"github.com/mattcheramie/nomaddev/internal/middleware"
	"github.com/mattcheramie/nomaddev/internal/sandbox"
	"github.com/mattcheramie/nomaddev/internal/session"
)

// lsp_query answers semantic code-navigation questions by talking to a real
// language server (gopls, typescript-language-server, pylsp). The server is a
// long-lived, session-scoped host process owned by s.lsp — so, like the
// monitor_daemon family, lsp_query bypasses the sandbox runner and the
// CompositeDispatcher and is handled here. Unlike a daemon the call is plain
// request/response: the handler runs the LSP operation and emits a single
// JSON command.chunk plus a terminal command.result. It is read-only, so it
// is never approval-gated.

// handleLSPCommand serves a direct client command.request for lsp_query. The
// work runs in a goroutine: an LSP operation can take several seconds (a cold
// server indexes the workspace first) and the read pump must stay free.
func (s *Server) handleLSPCommand(
	ctx context.Context, reqID string, p event.CommandRequestPayload,
	sess *session.Session, client *hub.Client,
) {
	go func() {
		started := time.Now()
		if err := middleware.Validate(p.Tool, p.Args); err != nil {
			s.emitResult(sess, client, reqID, started, -1, event.SandboxErrBadRequest, err.Error())
			return
		}
		if s.lsp == nil {
			s.emitResult(sess, client, reqID, started, -1, event.SandboxErrBadRequest,
				"lsp_query is not available: no workspace is configured")
			return
		}
		qctx, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() {
			select {
			case <-client.Done():
				cancel()
			case <-qctx.Done():
			}
		}()

		body, code, msg := s.executeLSPQuery(qctx, client.SID, p.Args)
		if code != "" {
			s.emitResult(sess, client, reqID, started, -1, code, msg)
			return
		}
		s.emitStdoutChunk(sess, client, reqID, string(body))
		s.emitResult(sess, client, reqID, started, 0, "", "")
	}()
}

// runLSPToolCall is the LLM-path counterpart of handleLSPCommand: invoked from
// runToolCall, it returns runToolCall's 5-tuple so the translator can resume.
// The JSON envelope is folded into ToolResult.Output, which is what the
// translation layer receives back.
func (s *Server) runLSPToolCall(
	ctx context.Context, pendingCmdID string, call middleware.ToolCall,
	sess *session.Session, client *hub.Client,
) (middleware.ToolResult, []byte, int, string, bool) {
	started := time.Now()

	fail := func(code, msg string) (middleware.ToolResult, []byte, int, string, bool) {
		s.emitResult(sess, client, pendingCmdID, started, -1, code, msg)
		output := map[string]any{"error_message": msg}
		_ = s.appendToolTurns(ctx, sess.SID, call, output, code, msg)
		return middleware.ToolResult{CallID: call.ID, Tool: call.Tool, Output: output, Error: code},
			nil, -1, msg, true
	}

	if s.lsp == nil {
		return fail(event.SandboxErrBadRequest,
			"lsp_query is not available: no workspace is configured")
	}

	body, code, msg := s.executeLSPQuery(ctx, client.SID, call.Args)
	if code != "" {
		return fail(code, msg)
	}

	s.emitStdoutChunk(sess, client, pendingCmdID, string(body))

	// Fold the LSP JSON envelope into the tool-result output so the
	// translator sees the locations/symbols directly.
	output := map[string]any{"exit_code": 0}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err == nil {
		for k, v := range parsed {
			output[k] = v
		}
	}
	s.emitResult(sess, client, pendingCmdID, started, 0, "", "")
	_ = s.appendToolTurns(ctx, sess.SID, call, output, "", "")
	return middleware.ToolResult{CallID: call.ID, Tool: call.Tool, Output: output},
		nil, 0, "", true
}

// executeLSPQuery resolves the language and target path, gets-or-starts the
// session's language server, runs the operation, and returns the JSON
// envelope. On failure it returns a SandboxErr* code and message.
func (s *Server) executeLSPQuery(
	ctx context.Context, sid string, args map[string]any,
) (body []byte, errCode, errMsg string) {
	operation, _ := args["operation"].(string)
	path, _ := args["path"].(string)
	lang, _ := args["lang"].(string)
	if lang == "" && path != "" {
		lang = sandbox.LangFromPath(path)
	}
	if lang == "" {
		return nil, event.SandboxErrBadRequest,
			"could not determine the language from the path; pass an explicit 'lang'"
	}

	q := sandbox.LSPQuery{
		Operation: operation,
		Line:      lspIntArg(args, "line"),
		Character: lspIntArg(args, "character"),
	}
	if sym, ok := args["query"].(string); ok {
		q.Symbol = sym
	}
	if inc, ok := args["include_declaration"].(bool); ok {
		q.IncludeDeclaration = inc
	}
	if path != "" {
		abs, err := s.lsp.ResolvePath(sid, path)
		if err != nil {
			return nil, event.SandboxErrBadRequest, err.Error()
		}
		q.AbsPath = abs
	}

	srv, err := s.lsp.GetOrStart(sid, lang)
	if err != nil {
		return nil, event.SandboxErrBadRequest, err.Error()
	}

	timeout := s.cfg.Sandbox.LSPRequestTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	qctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out, err := srv.Query(qctx, q, s.cfg.GitHub.MaxResultBytes)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, event.SandboxErrTimeout, "lsp_query timed out"
		case errors.Is(err, context.Canceled):
			return nil, event.SandboxErrCanceled, "client disconnected"
		default:
			return nil, event.SandboxErrInternal, err.Error()
		}
	}
	return out, "", ""
}

// lspIntArg reads an integer tool argument. JSON decode delivers numbers as
// float64; a Go-side caller may pass int.
func lspIntArg(args map[string]any, key string) int {
	switch n := args[key].(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}
