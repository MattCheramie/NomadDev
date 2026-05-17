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
	"github.com/mattcheramie/nomaddev/internal/metrics"
	"github.com/mattcheramie/nomaddev/internal/middleware"
	"github.com/mattcheramie/nomaddev/internal/sandbox"
	"github.com/mattcheramie/nomaddev/internal/session"
)

// Server bundles the HTTP listener, the WS upgrader, and references to the
// hub/session stores.
type Server struct {
	cfg       *config.Config
	log       *slog.Logger
	hub       *hub.Hub
	sessions  session.Store
	verifier  *auth.Verifier
	runner    sandbox.Runner      // may be nil — see handleCommandRequest
	mw        *middleware.Service // may be nil — see handleUserIntent
	sem       chan struct{}       // optional cap on concurrent execs; nil = unlimited
	intentSem chan struct{}       // optional cap on concurrent user.intent turns; nil = unlimited
	upgrader  websocket.Upgrader
	http      *http.Server
	mux       *http.ServeMux
}

// New constructs a Server. The HTTP server is built but not started. runner
// and mw may each be nil; when runner is nil the server replies to
// command.request with error{not_implemented}; when mw is nil the same
// applies to user.intent.
func New(
	cfg *config.Config, log *slog.Logger, h *hub.Hub, s session.Store,
	v *auth.Verifier, runner sandbox.Runner, mw *middleware.Service,
) *Server {
	mux := http.NewServeMux()
	srv := &Server{
		cfg:      cfg,
		log:      log,
		hub:      h,
		sessions: s,
		verifier: v,
		runner:   runner,
		mw:       mw,
		upgrader: websocket.Upgrader{
			Subprotocols: []string{"bearer"},
			CheckOrigin:  func(_ *http.Request) bool { return true },
		},
		mux: mux,
	}
	if runner != nil && cfg.Sandbox.MaxConcurrent > 0 {
		srv.sem = make(chan struct{}, cfg.Sandbox.MaxConcurrent)
	}
	if mw != nil && mw.Config.MaxConcurrent > 0 {
		srv.intentSem = make(chan struct{}, mw.Config.MaxConcurrent)
	}
	mux.HandleFunc("/healthz", srv.healthHandler)
	mux.Handle("/metrics", metrics.Handler())
	mux.HandleFunc("/ws", srv.wsHandler)
	if cfg.SPA.Enabled {
		// Registered AFTER /ws, /healthz, and /metrics so longest-prefix wins
		// keeps them resolving to their own handlers; "/" only matches when
		// nothing else does. spa_test.go pins this invariant.
		mux.Handle("/", srv.spaHandler())
	}
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
