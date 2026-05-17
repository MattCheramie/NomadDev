package wsserver

import (
	"log/slog"
	"time"

	"github.com/gorilla/websocket"

	"github.com/mattcheramie/nomaddev/internal/auth"
	"github.com/mattcheramie/nomaddev/internal/event"
	"github.com/mattcheramie/nomaddev/internal/hub"
	"github.com/mattcheramie/nomaddev/internal/metrics"
	"github.com/mattcheramie/nomaddev/internal/session"
)

// sendBuf is the per-client buffered Send channel size.
const sendBuf = 64

// runConnection wires the upgraded WS conn to the hub and pumps frames in both
// directions. It blocks until either pump returns, then tears the client down.
func (s *Server) runConnection(
	conn *websocket.Conn,
	clientID string,
	claims *auth.Claims,
	sess *session.Session,
	logger *slog.Logger,
) {
	client := hub.NewClient(clientID, claims.Sid, sendBuf)
	s.hub.Register(client)

	// Send hello synchronously through the session/buffer path so it's part of
	// the replay record.
	hello, err := event.NewEnvelope(event.EventHello, event.HelloPayload{
		SessionID:       claims.Sid,
		ServerTime:      time.Now().UTC().Format(time.RFC3339Nano),
		ProtocolVersion: event.ProtocolVersion,
	})
	if err == nil {
		s.bufferAndSend(sess, client, hello)
	}

	done := make(chan struct{})
	go s.writePump(conn, client, sess, logger, done)
	s.readPump(conn, client, sess, logger)
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
func (s *Server) readPump(
	conn *websocket.Conn,
	client *hub.Client,
	sess *session.Session,
	logger *slog.Logger,
) {
	defer client.Close()

	_ = conn.SetReadDeadline(time.Now().Add(s.cfg.ReadTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(s.cfg.ReadTimeout))
	})

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				logger.Info("ws: read closed", "err", err)
			}
			return
		}
		sess.Touch(time.Now().UTC())

		env, err := event.DecodeBytes(data)
		if err != nil {
			s.replyError(sess, client, "", event.CodeBadEnvelope, err.Error())
			continue
		}
		s.dispatch(env, client, sess, logger)
	}
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

// dispatch handles one decoded inbound envelope.
func (s *Server) dispatch(
	env event.Envelope,
	client *hub.Client,
	sess *session.Session,
	logger *slog.Logger,
) {
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
		s.handleCommandRequest(env, client, sess, logger)

	case event.EventUserIntent:
		s.handleUserIntent(env, client, sess, logger)

	case event.EventToolApprovalGranted:
		s.routeApproval(env, true)

	case event.EventToolApprovalDenied:
		s.routeApproval(env, false)

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
