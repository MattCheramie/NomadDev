package wsserver

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/mattcheramie/nomaddev/internal/event"
	"github.com/mattcheramie/nomaddev/internal/hub"
	"github.com/mattcheramie/nomaddev/internal/sandbox"
	"github.com/mattcheramie/nomaddev/internal/session"
)

// maxChunkSize bounds how much utf-8 we ship in a single command.chunk
// envelope. Larger output is split across consecutive chunks; the ring
// buffer's per-session byte cap (Session.MaxBytes) governs how far back a
// reconnecting client can replay.
const maxChunkSize = 16 * 1024

// handleCommandRequest is invoked by dispatch for an inbound command.request
// envelope. It is non-blocking: it spawns a goroutine that drives the runner
// and emits envelopes back over bufferAndSend, then returns immediately so
// the read pump can keep accepting frames.
func (s *Server) handleCommandRequest(
	env event.Envelope, client *hub.Client, sess *session.Session, logger *slog.Logger,
) {
	if s.runner == nil {
		s.replyError(sess, client, env.ID, event.CodeNotImplemented,
			"sandbox runner not configured")
		return
	}

	var p event.CommandRequestPayload
	if err := env.UnmarshalPayload(&p); err != nil {
		s.replyError(sess, client, env.ID, event.CodeBadEnvelope, err.Error())
		return
	}
	if p.Tool == "" {
		s.emitResult(sess, client, env.ID, time.Now(),
			-1, event.SandboxErrBadRequest, "missing tool")
		return
	}

	// Concurrency cap. Non-blocking acquire so a noisy session can't queue up
	// unbounded execs. The slot is released in the goroutine below.
	if s.sem != nil {
		select {
		case s.sem <- struct{}{}:
		default:
			s.emitResult(sess, client, env.ID, time.Now(),
				-1, event.SandboxErrUnavailable, "sandbox at capacity")
			return
		}
	}

	execCtx, cancel := context.WithCancel(context.Background())

	// Cancel the per-exec ctx when the client disconnects.
	go func() {
		select {
		case <-client.Done():
			cancel()
		case <-execCtx.Done():
		}
	}()

	go func() {
		defer cancel()
		if s.sem != nil {
			defer func() { <-s.sem }()
		}
		s.runExec(execCtx, env.ID, p, sess, client, logger)
	}()
}

// runExec drives the runner channel for one request, fanning chunks into
// command.chunk envelopes and emitting a terminal command.result.
func (s *Server) runExec(
	ctx context.Context, reqID string, p event.CommandRequestPayload,
	sess *session.Session, client *hub.Client, logger *slog.Logger,
) {
	started := time.Now()

	req := sandbox.ExecRequest{
		Tool:       p.Tool,
		Args:       p.Args,
		WorkingDir: p.WorkingDir,
		Timeout:    time.Duration(p.TimeoutMs) * time.Millisecond,
		Limits: sandbox.ResourceLimits{
			CPUNanos:    s.cfg.Sandbox.NanoCPUs,
			MemoryBytes: s.cfg.Sandbox.Memory,
			PidsLimit:   s.cfg.Sandbox.PidsLimit,
		},
	}
	if req.Timeout <= 0 {
		req.Timeout = s.cfg.Sandbox.DefaultTimeout
	}

	ch, err := s.runner.Exec(ctx, req)
	if err != nil {
		code := event.SandboxErrInternal
		if errors.Is(err, sandbox.ErrBadRequest) {
			code = event.SandboxErrBadRequest
		}
		s.emitResult(sess, client, reqID, started, -1, code, err.Error())
		return
	}

	seq := map[string]int{event.StreamStdout: 0, event.StreamStderr: 0}
	for chunk := range ch {
		switch chunk.Stream {
		case sandbox.StreamStdout, sandbox.StreamStderr:
			s.emitChunk(sess, client, reqID, chunk, seq)
		case sandbox.StreamExit:
			code, errCode, errMsg := classifyExit(chunk)
			s.emitResult(sess, client, reqID, started, code, errCode, errMsg)
			// Drain any straggler chunks (should not happen per contract).
			for range ch {
			}
			return
		default:
			logger.Warn("sandbox: unknown stream", "stream", chunk.Stream)
		}
	}
	// Channel closed without an exit chunk — contract violation. Emit a
	// synthetic failure so the client always sees a terminal frame.
	s.emitResult(sess, client, reqID, started, -1,
		event.SandboxErrInternal, "runner closed channel without exit")
}

// emitChunk slices chunk.Data into <=maxChunkSize utf-8 pieces and sends one
// command.chunk envelope per piece. It mutates seq in place.
func (s *Server) emitChunk(
	sess *session.Session, client *hub.Client, reqID string,
	chunk sandbox.ExecChunk, seq map[string]int,
) {
	if len(chunk.Data) == 0 {
		return
	}
	stream := chunk.Stream
	for off := 0; off < len(chunk.Data); off += maxChunkSize {
		end := off + maxChunkSize
		if end > len(chunk.Data) {
			end = len(chunk.Data)
		}
		data := strings.ToValidUTF8(string(chunk.Data[off:end]), "�")
		env, err := event.NewReply(event.EventCommandChunk, reqID, event.CommandChunkPayload{
			Stream: stream,
			Seq:    seq[stream],
			Data:   data,
		})
		seq[stream]++
		if err != nil {
			s.log.Error("sandbox: build chunk envelope", "err", err)
			return
		}
		s.bufferAndSend(sess, client, env)
	}
}

// emitResult builds and sends the single command.result envelope.
func (s *Server) emitResult(
	sess *session.Session, client *hub.Client, reqID string,
	started time.Time, exitCode int, errCode, errMsg string,
) {
	env, err := event.NewReply(event.EventCommandResult, reqID, event.CommandResultPayload{
		ExitCode:     exitCode,
		DurationMs:   time.Since(started).Milliseconds(),
		Error:        errCode,
		ErrorMessage: errMsg,
	})
	if err != nil {
		s.log.Error("sandbox: build result envelope", "err", err)
		return
	}
	s.bufferAndSend(sess, client, env)
}

// classifyExit translates an exit chunk into (exit_code, error_code, message).
func classifyExit(c sandbox.ExecChunk) (int, string, string) {
	if c.Err == nil {
		return c.ExitCode, "", ""
	}
	switch {
	case errors.Is(c.Err, context.DeadlineExceeded):
		return -1, event.SandboxErrTimeout, "exec timed out"
	case errors.Is(c.Err, context.Canceled), errors.Is(c.Err, sandbox.ErrCanceled):
		return -1, event.SandboxErrCanceled, "client disconnected"
	case errors.Is(c.Err, sandbox.ErrBadRequest):
		return -1, event.SandboxErrBadRequest, c.Err.Error()
	case errors.Is(c.Err, sandbox.ErrOOM):
		return -1, event.SandboxErrOOM, c.Err.Error()
	case errors.Is(c.Err, sandbox.ErrImagePull):
		return -1, event.SandboxErrImagePull, c.Err.Error()
	}
	return -1, event.SandboxErrInternal, c.Err.Error()
}
