package wsserver

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/mattcheramie/nomaddev/internal/auth"
	"github.com/mattcheramie/nomaddev/internal/config"
	"github.com/mattcheramie/nomaddev/internal/hub"
	"github.com/mattcheramie/nomaddev/internal/session"
)

// Phase-10 hardening: CheckOrigin allowlist + CSP/security headers
// on the SPA. Build minimal test servers so each behavior can be
// asserted in isolation.

func buildOriginServer(t *testing.T, allowed []string) (*httptest.Server, *auth.IssuerSigner) {
	t.Helper()
	cfg := &config.Config{
		ListenAddr:     "127.0.0.1:0",
		JWTSecret:      []byte(testSecret),
		AllowedOrigins: allowed,
		LogLevel:       slog.LevelInfo,
		Session:        config.SessionConfig{BufferSize: 32, MaxBytes: 1 << 20},
		Sandbox:        config.SandboxConfig{DefaultTimeout: 2 * time.Second, MaxConcurrent: 4},
		Approval:       config.ApprovalConfig{Timeout: 2 * time.Second},
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   2 * time.Second,
		PingInterval:   30 * time.Second,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := hub.New(logger)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go h.Run(ctx)
	sessions := session.NewMemoryStore(cfg.Session.BufferSize, cfg.Session.MaxBytes)
	verifier := auth.NewVerifier(cfg.JWTSecret)
	srv := New(cfg, logger, h, sessions, verifier, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	issuer := auth.NewIssuer(cfg.JWTSecret, time.Hour)
	return ts, issuer
}

func TestBuildOriginChecker_EmptyAllowlistAcceptsAny(t *testing.T) {
	fn := buildOriginChecker(nil, nil)
	r := httptest.NewRequest(http.MethodGet, "/ws", nil)
	r.Header.Set("Origin", "https://random.example")
	if !fn(r) {
		t.Fatal("empty allowlist must accept any origin (pre-Phase-10 behavior)")
	}
}

func TestBuildOriginChecker_NonEmptyAllowlistMatchesExactly(t *testing.T) {
	fn := buildOriginChecker([]string{"https://nomaddev.example", "http://localhost:8080"}, nil)
	cases := []struct {
		origin string
		want   bool
	}{
		{"https://nomaddev.example", true},
		{"HTTPS://NOMADDEV.EXAMPLE", true}, // case-insensitive
		{"http://localhost:8080", true},
		{"https://evil.example", false},
		{"http://localhost:9999", false},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "/ws", nil)
		r.Header.Set("Origin", tc.origin)
		if got := fn(r); got != tc.want {
			t.Errorf("origin %q: got %v, want %v", tc.origin, got, tc.want)
		}
	}
}

func TestBuildOriginChecker_NoOriginHeaderPasses(t *testing.T) {
	// Non-browser clients (wsclient, curl) don't send Origin. The
	// JWT gate at /ws is the real auth boundary; CSRF only applies
	// to browser-driven origin-carrying requests.
	fn := buildOriginChecker([]string{"https://only-this.example"}, nil)
	r := httptest.NewRequest(http.MethodGet, "/ws", nil)
	// No Origin header.
	if !fn(r) {
		t.Fatal("requests without Origin must pass even when allowlist is set")
	}
}

func TestWSHandshake_RejectsDisallowedOrigin(t *testing.T) {
	ts, issuer := buildOriginServer(t, []string{"https://only-this.example"})
	tok, _ := issuer.Sign("matt", "sess-1", nil)

	// gorilla.Dialer doesn't set an Origin by default; explicitly
	// set one that's NOT in the allowlist to trip the rejection.
	hdrs := http.Header{}
	hdrs.Set("Authorization", "Bearer "+tok)
	hdrs.Set("Origin", "https://attacker.example")
	url := "ws" + ts.URL[len("http"):] + "/ws"
	_, resp, err := websocket.DefaultDialer.Dial(url, hdrs)
	if err == nil {
		t.Fatal("expected dial to fail with disallowed Origin")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %v, want 403 Forbidden", resp)
	}
}

func TestSecurityHeaders_AreApplied(t *testing.T) {
	// Send a request through withSecurityHeaders directly; the SPA
	// is wired through this wrapper in the prod server, but
	// constructing a full SPA server with embed assets is heavier
	// than this unit-level check needs.
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	wrapped := withSecurityHeaders(inner)

	w := httptest.NewRecorder()
	wrapped.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	for _, h := range []string{
		"Content-Security-Policy",
		"X-Content-Type-Options",
		"Referrer-Policy",
		"X-Frame-Options",
	} {
		if got := w.Header().Get(h); got == "" {
			t.Errorf("missing header %s", h)
		}
	}
	if got := w.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q", got)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
}
