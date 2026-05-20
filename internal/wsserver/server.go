// Package wsserver hosts the HTTP server that upgrades to WebSocket and runs
// the per-connection read/write pumps for the orchestrator.
package wsserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/mattcheramie/nomaddev/internal/audit"
	"github.com/mattcheramie/nomaddev/internal/auth"
	"github.com/mattcheramie/nomaddev/internal/config"
	"github.com/mattcheramie/nomaddev/internal/hub"
	"github.com/mattcheramie/nomaddev/internal/metrics"
	"github.com/mattcheramie/nomaddev/internal/middleware"
	"github.com/mattcheramie/nomaddev/internal/sandbox"
	"github.com/mattcheramie/nomaddev/internal/session"
	"github.com/mattcheramie/nomaddev/internal/webauthn"
)

// Server bundles the HTTP listener, the WS upgrader, and references to the
// hub/session stores.
type Server struct {
	cfg       *config.Config
	log       *slog.Logger
	hub       *hub.Hub
	sessions  session.Store
	verifier  *auth.Verifier
	issuer    *auth.IssuerSigner  // nil disables /auth/refresh
	revoker   auth.RevocationList // nil disables /auth/revoke and revocation checks
	audit     audit.Sink          // nil falls back to audit.NoopSink at use sites
	webauthn  *webauthn.Service   // nil disables /auth/webauthn/*
	readiness []ReadinessProbe    // probed by /readyz
	runner    sandbox.Runner      // may be nil — see handleCommandRequest
	mw        *middleware.Service // may be nil — see handleUserIntent
	sem       chan struct{}       // optional cap on concurrent execs; nil = unlimited
	intentSem chan struct{}       // optional cap on concurrent user.intent turns; nil = unlimited
	// modelOverrides holds per-SID translator-model overrides driven by
	// the mobile Settings picker (user.command{set_model}). Lookup is
	// O(1) on every user.intent so the override path stays cheap. Cleared
	// on reset_history so a fresh session goes back to the server default.
	modelOverrides sync.Map // map[string]string — SID → model name
	upgrader       websocket.Upgrader
	http           *http.Server
	mux            *http.ServeMux
}

// ReadinessProbe is one named dependency the /readyz handler will
// probe. Check is invoked with a per-request context; a nil return
// means healthy, any error returns 503 for the whole endpoint.
type ReadinessProbe struct {
	Name  string
	Check func(ctx context.Context) error
}

// Options carries optional dependencies for New. Existing callers that
// pass nil for both Issuer and Revoker get the pre-refresh behavior
// (HTTP /ws only, no /auth/* endpoints).
type Options struct {
	Issuer  *auth.IssuerSigner
	Revoker auth.RevocationList
	// Audit is the structured security-event sink. nil is equivalent
	// to audit.NoopSink — every audit.Log call becomes a no-op.
	Audit audit.Sink
	// Readiness probes are invoked by /readyz, one per configured
	// dependency. Empty slice = /readyz always reports OK (acceptable
	// for the no-deps mock-runtime path).
	Readiness []ReadinessProbe
	// WebAuthn enables the /auth/webauthn/* endpoints. nil leaves
	// them unregistered (operators on plain-HTTP Tailscale deploys
	// where WebAuthn can't satisfy its HTTPS-or-localhost
	// requirement don't pay for routes they can't use).
	WebAuthn *webauthn.Service
}

// New constructs a Server. The HTTP server is built but not started. runner
// and mw may each be nil; when runner is nil the server replies to
// command.request with error{not_implemented}; when mw is nil the same
// applies to user.intent.
func New(
	cfg *config.Config, log *slog.Logger, h *hub.Hub, s session.Store,
	v *auth.Verifier, runner sandbox.Runner, mw *middleware.Service,
) *Server {
	return NewWithOptions(cfg, log, h, s, v, runner, mw, Options{})
}

// NewWithOptions is New plus the auth Issuer/Revoker hookups needed by
// the /auth/refresh and /auth/revoke endpoints.
func NewWithOptions(
	cfg *config.Config, log *slog.Logger, h *hub.Hub, s session.Store,
	v *auth.Verifier, runner sandbox.Runner, mw *middleware.Service,
	opts Options,
) *Server {
	mux := http.NewServeMux()
	srv := &Server{
		cfg:       cfg,
		log:       log,
		hub:       h,
		sessions:  s,
		verifier:  v,
		issuer:    opts.Issuer,
		revoker:   opts.Revoker,
		audit:     coalesceAudit(opts.Audit),
		readiness: opts.Readiness,
		webauthn:  opts.WebAuthn,
		runner:    runner,
		mw:        mw,
		upgrader: websocket.Upgrader{
			Subprotocols: []string{"bearer"},
			CheckOrigin:  buildOriginChecker(cfg.AllowedOrigins, log),
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
	mux.HandleFunc("/readyz", srv.readyHandler)
	mux.Handle("/metrics", metrics.Handler())
	mux.HandleFunc("/ws", srv.wsHandler)
	if opts.Issuer != nil {
		mux.HandleFunc("/auth/refresh", srv.refreshHandler)
	}
	if opts.Revoker != nil {
		mux.HandleFunc("/auth/revoke", srv.revokeHandler)
	}
	if opts.WebAuthn != nil {
		mux.HandleFunc("/auth/webauthn/register/begin", srv.webauthnRegisterBeginHandler)
		mux.HandleFunc("/auth/webauthn/register/finish", srv.webauthnRegisterFinishHandler)
		mux.HandleFunc("/auth/webauthn/login/begin", srv.webauthnLoginBeginHandler)
		mux.HandleFunc("/auth/webauthn/login/finish", srv.webauthnLoginFinishHandler)
	}
	if cfg.SPA.Enabled {
		// Registered AFTER /ws, /healthz, and /metrics so longest-prefix wins
		// keeps them resolving to their own handlers; "/" only matches when
		// nothing else does. spa_test.go pins this invariant.
		mux.Handle("/", withSecurityHeaders(srv.spaHandler()))
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
	// Liveness: the process is up and the HTTP listener is responding.
	// Does NOT probe dependencies — that's /readyz's job.
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// readyHandler implements /readyz. Iterates the configured Readiness
// probes and returns 200 with per-probe pass/fail JSON; 503 if any
// single probe failed. Each probe gets its own 2-second deadline so a
// hung dependency can't lock the endpoint open. The JSON shape is
// stable: {"status":"ok|degraded","checks":{"name":"ok|err msg",...}}.
func (s *Server) readyHandler(w http.ResponseWriter, r *http.Request) {
	checks := make(map[string]string, len(s.readiness))
	allOK := true
	for _, p := range s.readiness {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		err := p.Check(ctx)
		cancel()
		if err != nil {
			allOK = false
			checks[p.Name] = err.Error()
			continue
		}
		checks[p.Name] = "ok"
	}
	status := "ok"
	code := http.StatusOK
	if !allOK {
		status = "degraded"
		code = http.StatusServiceUnavailable
	}
	body := readyResponse{Status: status, Checks: checks}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// readyResponse is the JSON shape returned by /readyz. Keep keys
// stable — downstream alerting depends on them.
type readyResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

// coalesceAudit returns the supplied Sink or a NoopSink if nil. Every
// audit call site goes through s.audit.Log directly so they don't have
// to nil-check.
func coalesceAudit(s audit.Sink) audit.Sink {
	if s == nil {
		return audit.NoopSink{}
	}
	return s
}

// buildOriginChecker returns the websocket.Upgrader.CheckOrigin func
// driven by NOMADDEV_WS_ALLOWED_ORIGINS. Empty allowlist preserves
// the pre-Phase-10 behavior of accepting any origin (the default
// Tailscale-fronted deploy doesn't have a meaningful origin
// boundary). A non-empty allowlist hard-rejects every request whose
// Origin header isn't an exact, case-insensitive match — operators
// who terminate TLS at a reverse proxy get a clear CSRF
// boundary at /ws.
//
// Same-origin requests (no Origin header) and unconditional clients
// like the wsclient test driver continue to pass; the upstream
// gorilla/websocket library never strips Origin for these.
func buildOriginChecker(allowed []string, log *slog.Logger) func(*http.Request) bool {
	if len(allowed) == 0 {
		return func(*http.Request) bool { return true }
	}
	// Lowercase once at construction so the per-request hot path is
	// a single map lookup.
	set := make(map[string]struct{}, len(allowed))
	for _, o := range allowed {
		set[strings.ToLower(strings.TrimSpace(o))] = struct{}{}
	}
	return func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			// No Origin = same-origin or non-browser client. The JWT
			// gate on /ws is the real auth boundary; cross-origin
			// CSRF concerns only apply to browser-driven requests
			// that carry an Origin header.
			return true
		}
		if _, ok := set[strings.ToLower(origin)]; ok {
			return true
		}
		if log != nil {
			log.Warn("ws: rejecting origin", "origin", origin, "remote", r.RemoteAddr)
		}
		return false
	}
}

// withSecurityHeaders wraps a Handler so every response carries the
// hardening headers we ship for the SPA route. Applied to the SPA
// handler only; /ws and /metrics keep their existing shapes (the WS
// handshake doesn't honor CSP and Prometheus scrape paths shouldn't
// pretend to be a browser context).
//
// The CSP is intentionally narrow: 'self' for scripts and styles,
// 'self' plus the WebSocket scheme for connect-src so the SPA can
// keep its single /ws upgrade. 'unsafe-inline' on style-src is
// concession to React Native Web's runtime style emission — Expo
// inlines computed styles; tightening that needs an SPA-side
// build change that's out of scope for this phase.
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self'; "+
				"style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; "+
				"connect-src 'self' ws: wss:; "+
				"frame-ancestors 'none'; "+
				"base-uri 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}
