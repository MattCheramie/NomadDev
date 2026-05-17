package wsserver

import (
	"net/http"
	"strings"
	"time"

	"github.com/mattcheramie/nomaddev/internal/event"
)

// wsHandler is the /ws endpoint. It validates the JWT BEFORE upgrading so
// rejected clients see a plain HTTP 401 instead of a WS close frame.
func (s *Server) wsHandler(w http.ResponseWriter, r *http.Request) {
	token := extractToken(r)
	if token == "" {
		s.log.Warn("ws: missing token", "remote", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	claims, err := s.verifier.Parse(token)
	if err != nil {
		s.log.Warn("ws: token rejected", "remote", r.RemoteAddr, "err", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote a response on error.
		s.log.Warn("ws: upgrade failed", "remote", r.RemoteAddr, "err", err)
		return
	}

	clientID := event.NewID()
	logger := s.log.With(
		"client_id", clientID,
		"sid", claims.Sid,
		"sub", claims.Sub,
		"remote", r.RemoteAddr,
	)
	logger.Info("ws: connected")

	sess := s.sessions.GetOrCreate(claims.Sid)
	sess.Touch(time.Now().UTC())

	s.runConnection(conn, clientID, claims, sess, logger)
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

