package wsserver

import (
	"context"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/mattcheramie/nomaddev/internal/audit"
	"github.com/mattcheramie/nomaddev/internal/event"
	"github.com/mattcheramie/nomaddev/internal/metrics"
)

// wsHandler is the /ws endpoint. It validates the JWT BEFORE upgrading so
// rejected clients see a plain HTTP 401 instead of a WS close frame.
func (s *Server) wsHandler(w http.ResponseWriter, r *http.Request) {
	token := extractToken(r)
	if token == "" {
		s.log.Warn("ws: missing token", "remote", r.RemoteAddr)
		metrics.WSConnectsTotal.WithLabelValues("unauthorized").Inc()
		s.audit.Log(r.Context(), audit.Event{
			Kind: audit.KindWSAuthFailed, Outcome: audit.OutcomeError,
			Remote: r.RemoteAddr, Message: "missing token",
		})
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	claims, err := s.verifier.ParseAccess(token)
	if err != nil {
		s.log.Warn("ws: token rejected", "remote", r.RemoteAddr, "err", err)
		metrics.WSConnectsTotal.WithLabelValues("unauthorized").Inc()
		s.audit.Log(r.Context(), audit.Event{
			Kind: audit.KindWSAuthFailed, Outcome: audit.OutcomeError,
			Remote: r.RemoteAddr, Message: err.Error(),
		})
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote a response on error.
		s.log.Warn("ws: upgrade failed", "remote", r.RemoteAddr, "err", err)
		metrics.WSConnectsTotal.WithLabelValues("upgrade_failed").Inc()
		return
	}
	metrics.WSConnectsTotal.WithLabelValues("ok").Inc()
	metrics.WSActiveConnections.Inc()
	defer metrics.WSActiveConnections.Dec()

	clientID := event.NewID()
	logger := s.log.With(
		"client_id", clientID,
		"sid", claims.Sid,
		"sub", claims.Sub,
		"remote", r.RemoteAddr,
	)
	logger.Info("ws: connected")
	s.audit.Log(r.Context(), audit.Event{
		Kind: audit.KindWSConnect, Outcome: audit.OutcomeOK,
		Sub: claims.Sub, Sid: claims.Sid, Remote: r.RemoteAddr, JTI: claims.ID,
	})

	sess := s.sessions.GetOrCreate(claims.Sid)
	sess.Touch(time.Now().UTC())

	// Phase 11.4: extract `traceparent` (W3C trace context) from the
	// upgrade headers BEFORE the connection's lifetime begins. The
	// returned context carries the remote trace + span ID; per-envelope
	// dispatch spans link to it as parents, so a Playwright /
	// otel-instrumented browser client sees the orchestrator's spans
	// in the same trace.
	//
	// Detached from r.Context() because the request is gone the moment
	// Upgrade returns — the connection outlives the HTTP request.
	connCtx := otel.GetTextMapPropagator().Extract(
		context.Background(), propagation.HeaderCarrier(r.Header))

	s.runConnection(connCtx, conn, clientID, claims, sess, logger)
}

// extractToken pulls the JWT from either the bearer subprotocol or the
// Authorization header. Subprotocol form: `Sec-WebSocket-Protocol: bearer, <token>`.
func extractToken(r *http.Request) string {
	if h := r.Header.Get("Sec-WebSocket-Protocol"); h != "" {
		for _, part := range strings.Split(h, ",") {
			p := strings.TrimSpace(part)
			if p == "" || strings.EqualFold(p, "bearer") {
				continue
			}
			return p
		}
	}
	if h := r.Header.Get("Authorization"); h != "" {
		if strings.HasPrefix(strings.ToLower(h), "bearer ") {
			return strings.TrimSpace(h[len("Bearer "):])
		}
	}
	return ""
}
