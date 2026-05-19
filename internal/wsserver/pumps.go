package wsserver

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/time/rate"

	"github.com/mattcheramie/nomaddev/internal/auth"
	"github.com/mattcheramie/nomaddev/internal/event"
	"github.com/mattcheramie/nomaddev/internal/hub"
	"github.com/mattcheramie/nomaddev/internal/metrics"
	"github.com/mattcheramie/nomaddev/internal/middleware"
	"github.com/mattcheramie/nomaddev/internal/session"
)

// sendBuf is the per-client buffered Send channel size.
const sendBuf = 64

// runConnection wires the upgraded WS conn to the hub and pumps frames in both
// directions. It blocks until either pump returns, then tears the client down.
// connCtx carries the W3C trace context extracted from the upgrade
// headers (Phase 11.4) — each per-envelope dispatch span links to
// it as parent.
func (s *Server) runConnection(
	connCtx context.Context,
	conn *websocket.Conn,
	clientID string,
	claims *auth.Claims,
	sess *session.Session,
	logger *slog.Logger,
) {
	client := hub.NewClientWithScopes(clientID, claims.Sid, claims.Sub, claims.Scopes, sendBuf)
	s.hub.Register(client)

	// Send hello synchronously through the session/buffer path so it's part of
	// the replay record. Provider / model / available_models are populated
	// only when a non-mock middleware Service is wired — mock and "none"
	// runtimes ship a hello identical to the pre-model-switch shape so
	// existing tests stay byte-stable.
	helloPayload := event.HelloPayload{
		SessionID:       claims.Sid,
		ServerTime:      time.Now().UTC().Format(time.RFC3339Nano),
		ProtocolVersion: event.ProtocolVersion,
	}
	if s.mw != nil && s.mw.Config.Provider != "" {
		helloPayload.Provider = s.mw.Config.Provider
		// Use the effective model so a per-session override applied on a
		// prior connection (and re-applied by the client on reconnect)
		// shows the correct initial selection in the picker.
		helloPayload.Model = s.effectiveModel(claims.Sid)
		if helloPayload.Model == "" {
			helloPayload.Model = s.mw.Config.Model
		}
		helloPayload.AvailableModels = middleware.ModelsForProvider(s.mw.Config.Provider)
	}
	hello, err := event.NewEnvelope(event.EventHello, helloPayload)
	if err == nil {
		s.bufferAndSend(sess, client, hello)
	}

	done := make(chan struct{})
	go s.writePump(conn, client, sess, logger, done)
	s.readPump(connCtx, conn, client, sess, logger)
	<-done

	s.hub.Unregister(client)
	_ = conn.Close()
	logger.Info("ws: disconnected")
}

// bufferAndSend appends env to the session buffer and enqueues it for the
// write pump. Buffer write happens first so that a slow client can't lose
// state on a Send channel overflow. The non-blocking send means a dead or
// gone client never blocks; Done() is checked alongside Send so the goroutine
// driving this can wind down cleanly when the connection drops.
func (s *Server) bufferAndSend(sess *session.Session, c *hub.Client, env event.Envelope) {
	b, err := env.Bytes()
	if err != nil {
		s.log.Error("ws: marshal envelope", "err", err)
		return
	}
	sess.Append(env, len(b))
	metrics.SessionEventsTotal.WithLabelValues(env.Type).Inc()
	select {
	case c.Send <- env:
	case <-c.Done():
	default:
		s.log.Warn("ws: client send buffer full, dropping", "client_id", c.ID, "type", env.Type)
	}
}

// readPump reads inbound frames, dispatches them, and exits on any error.
//
// Two policies guard the dispatcher from a hostile or runaway client:
//
//   - SetReadLimit caps each frame at s.cfg.MaxMessageBytes. Gorilla
//     enforces the limit and closes the connection with 1009 Message
//     Too Big on violation; we emit one error envelope first so the
//     client sees a structured reason instead of a bare close frame.
//   - A per-connection token-bucket rate limiter rejects envelopes
//     once the steady-state rate is exceeded. Rejected frames get an
//     error{rate_limited} envelope but the connection stays open — a
//     well-behaved client can throttle and resume.
func (s *Server) readPump(
	connCtx context.Context,
	conn *websocket.Conn,
	client *hub.Client,
	sess *session.Session,
	logger *slog.Logger,
) {
	defer client.Close()

	if s.cfg.MaxMessageBytes > 0 {
		conn.SetReadLimit(s.cfg.MaxMessageBytes)
	}
	_ = conn.SetReadDeadline(time.Now().Add(s.cfg.ReadTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(s.cfg.ReadTimeout))
	})

	var limiter *rate.Limiter
	if s.cfg.RateLimit > 0 && s.cfg.RateBurst > 0 {
		limiter = rate.NewLimiter(rate.Limit(s.cfg.RateLimit), s.cfg.RateBurst)
	}

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			// gorilla/websocket's SetReadLimit closes the connection
			// with a 1009 close frame BEFORE ReadMessage returns; we
			// can't push an error envelope back through the same conn
			// at this point. Surface via metric + structured log so
			// operators see the bound being hit. Well-behaved clients
			// observe the 1009 close code and back off.
			if isMessageTooBig(err) {
				logger.Warn("ws: inbound frame exceeded limit",
					"limit_bytes", s.cfg.MaxMessageBytes, "err", err)
				metrics.WSInboundRejectedTotal.WithLabelValues("message_too_large").Inc()
			} else if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				logger.Info("ws: read closed", "err", err)
			}
			return
		}
		sess.Touch(time.Now().UTC())

		if limiter != nil && !limiter.Allow() {
			metrics.WSInboundRejectedTotal.WithLabelValues("rate_limited").Inc()
			s.replyError(sess, client, "", event.CodeRateLimited,
				"connection-level rate limit exceeded")
			continue
		}

		env, err := event.DecodeBytes(data)
		if err != nil {
			s.replyError(sess, client, "", event.CodeBadEnvelope, err.Error())
			continue
		}
		s.dispatch(connCtx, env, client, sess, logger)
	}
}

// isMessageTooBig reports whether err looks like a ReadLimit overflow
// from gorilla/websocket. The library wraps the original Go error with
// "websocket: read limit exceeded"; CloseError 1009 fires on the
// remote-disconnect path. Both should be treated identically.
func isMessageTooBig(err error) bool {
	if err == nil {
		return false
	}
	var ce *websocket.CloseError
	if errors.As(err, &ce) && ce.Code == websocket.CloseMessageTooBig {
		return true
	}
	// Conservative substring check — gorilla's "read limit" message
	// is stable across the v1.5.x line we depend on.
	return strings.Contains(err.Error(), "read limit")
}

// writePump multiplexes between the client's Send channel and a heartbeat
// timer. Returns (via the done channel) on any write failure.
func (s *Server) writePump(
	conn *websocket.Conn,
	client *hub.Client,
	_ *session.Session,
	logger *slog.Logger,
	done chan<- struct{},
) {
	defer close(done)

	ticker := time.NewTicker(s.cfg.PingInterval)
	defer ticker.Stop()

	for {
		select {
		case env := <-client.Send:
			_ = conn.SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout))
			b, err := env.Bytes()
			if err != nil {
				logger.Error("ws: marshal outbound", "err", err)
				continue
			}
			if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
				logger.Info("ws: write closed", "err", err)
				return
			}

		case <-ticker.C:
			_ = conn.SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				logger.Info("ws: heartbeat failed", "err", err)
				return
			}

		case <-client.Done():
			_ = conn.SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout))
			_ = conn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			return
		}
	}
}

// dispatch handles one decoded inbound envelope. connCtx carries the
// W3C trace context extracted at upgrade time (Phase 11.4); the
// returned ctx is the dispatch-span ctx, which downstream handlers
// thread into runner.Exec / Client.Call so per-tool child spans
// chain under the dispatch root.
func (s *Server) dispatch(
	connCtx context.Context,
	env event.Envelope,
	client *hub.Client,
	sess *session.Session,
	logger *slog.Logger,
) {
	// Phase 11.2 / 11.4: one root span per inbound envelope, parented
	// to whatever traceparent the upgrade request carried (no-op when
	// tracing is disabled). Span surfaces type / sub / sid as
	// attributes so a collector can filter by tool or operator
	// without sampling-tax on quiet envelopes (a single tracer Start
	// is ~tens of ns when the noop provider is installed).
	tracer := otel.Tracer("nomaddev/wsserver")
	dispatchCtx, span := tracer.Start(connCtx, "ws.dispatch."+env.Type,
		trace.WithAttributes(
			attribute.String("envelope.type", env.Type),
			attribute.String("session.sub", client.Sub),
			attribute.String("session.sid", client.SID),
		),
	)
	defer span.End()

	switch env.Type {
	case event.EventClientHello:
		var p event.ClientHelloPayload
		if err := env.UnmarshalPayload(&p); err != nil {
			s.replyError(sess, client, env.ID, event.CodeBadEnvelope, err.Error())
			return
		}
		if p.LastEventID == "" {
			return
		}
		replay, stale := sess.EventsSince(p.LastEventID)
		if stale {
			first, last := sess.BufferBounds()
			payload := event.SessionStalePayload{
				Reason:          "buffer_rolled",
				FirstBufferedID: first,
				LastBufferedID:  last,
			}
			reply, err := event.NewEnvelope(event.EventSessionStale, payload)
			if err == nil {
				s.bufferAndSend(sess, client, reply)
			}
			return
		}
		for _, e := range replay {
			// Replay does NOT re-buffer; the entries are already in the buffer.
			select {
			case client.Send <- e:
			default:
				logger.Warn("ws: replay buffer full")
				return
			}
		}

	case event.EventPing:
		var p event.PingPayload
		_ = env.UnmarshalPayload(&p)
		pong, err := event.NewReply(event.EventPong, env.ID, p)
		if err == nil {
			s.bufferAndSend(sess, client, pong)
		}

	case event.EventCommandRequest:
		s.handleCommandRequest(dispatchCtx, env, client, sess, logger)

	case event.EventUserIntent:
		s.handleUserIntent(dispatchCtx, env, client, sess, logger)

	case event.EventToolApprovalGranted:
		s.routeApproval(env, client, true)

	case event.EventToolApprovalDenied:
		s.routeApproval(env, client, false)

	case event.EventUserCommand:
		s.handleUserCommand(env, client, sess, logger)

	default:
		s.replyError(sess, client, env.ID, event.CodeUnknownType,
			"unsupported event type: "+env.Type)
	}
}

func (s *Server) replyError(
	sess *session.Session,
	client *hub.Client,
	correlationID, code, message string,
) {
	env, err := event.NewReply(event.EventError, correlationID, event.ErrorPayload{
		Code:    code,
		Message: message,
	})
	if err != nil {
		return
	}
	s.bufferAndSend(sess, client, env)
}
