// Package wsserver hosts the HTTP server that upgrades to WebSocket and runs
// the per-connection read/write pumps for the orchestrator.
package wsserver

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"github.com/mattcheramie/nomaddev/internal/auth"
	"github.com/mattcheramie/nomaddev/internal/config"
	"github.com/mattcheramie/nomaddev/internal/hub"
	"github.com/mattcheramie/nomaddev/internal/session"
)

// Server bundles the HTTP listener, the WS upgrader, and references to the
// hub/session stores.
type Server struct {
	cfg      *config.Config
	log      *slog.Logger
	hub      *hub.Hub
	sessions session.Store
	verifier *auth.Verifier
	upgrader websocket.Upgrader
	http     *http.Server
	mux      *http.ServeMux
}

// New constructs a Server. The HTTP server is built but not started.
func New(cfg *config.Config, log *slog.Logger, h *hub.Hub, s session.Store, v *auth.Verifier) *Server {
	mux := http.NewServeMux()
	srv := &Server{
		cfg:      cfg,
		log:      log,
		hub:      h,
		sessions: s,
		verifier: v,
		upgrader: websocket.Upgrader{
			Subprotocols: []string{"bearer"},
			CheckOrigin:  func(_ *http.Request) bool { return true },
		},
		mux: mux,
	}
	mux.HandleFunc("/healthz", srv.healthHandler)
	mux.HandleFunc("/ws", srv.wsHandler)
	srv.http = &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv
}

// Addr returns the configured listen address.
func (s *Server) Addr() string { return s.http.Addr }

// SetAddr overrides the listen address (used by tests that bind :0).
func (s *Server) SetAddr(addr string) { s.http.Addr = addr }

// Handler returns the underlying http.Handler (used by httptest).
func (s *Server) Handler() http.Handler { return s.mux }

// ListenAndServe binds and serves until Shutdown or fatal error.
func (s *Server) ListenAndServe() error {
	return s.http.ListenAndServe()
}

// Shutdown gracefully drains in-flight requests, bounded by ctx.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

func (s *Server) healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}
