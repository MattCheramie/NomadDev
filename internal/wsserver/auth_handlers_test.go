package wsserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mattcheramie/nomaddev/internal/auth"
	"github.com/mattcheramie/nomaddev/internal/config"
	"github.com/mattcheramie/nomaddev/internal/hub"
	"github.com/mattcheramie/nomaddev/internal/session"
)

// authTestServer wires a Server with the new auth options (issuer +
// in-memory revocation) on top of the existing test scaffolding.
func authTestServer(t *testing.T) (*httptest.Server, *auth.IssuerSigner, auth.RevocationList) {
	t.Helper()
	cfg := &config.Config{
		ListenAddr:   "127.0.0.1:0",
		JWTSecret:    []byte(testSecret),
		LogLevel:     slog.LevelInfo,
		Session:      config.SessionConfig{BufferSize: 32, MaxBytes: 1 << 20},
		Sandbox:      config.SandboxConfig{DefaultTimeout: 2 * time.Second, MaxConcurrent: 4},
		Approval:     config.ApprovalConfig{Timeout: 2 * time.Second},
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 2 * time.Second,
		PingInterval: 30 * time.Second,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := hub.New(logger)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go h.Run(ctx)

	sessions := session.NewMemoryStore(cfg.Session.BufferSize, cfg.Session.MaxBytes)
	revoker := auth.NewMemoryRevocationList()
	verifier := auth.NewVerifierWithRevocation(cfg.JWTSecret, revoker)
	issuer := auth.NewIssuerWithTTLs(cfg.JWTSecret, 15*time.Minute, time.Hour)

	srv := NewWithOptions(cfg, logger, h, sessions, verifier, nil, nil, Options{
		Issuer:  issuer,
		Revoker: revoker,
	})

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, issuer, revoker
}

func postJSON(t *testing.T, url string, headers map[string]string, body any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, err := http.NewRequest(http.MethodPost, url, &buf)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

func TestAuth_RefreshFlow_MintsNewPair(t *testing.T) {
	ts, issuer, revoker := authTestServer(t)
	refresh, err := issuer.SignRefresh("matt", "sess-1", []string{"orchestrator:connect"})
	if err != nil {
		t.Fatalf("SignRefresh: %v", err)
	}

	resp := postJSON(t, ts.URL+"/auth/refresh", map[string]string{
		"Authorization": "Bearer " + refresh,
	}, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	var got refreshResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.AccessToken == "" || got.RefreshToken == "" {
		t.Fatalf("tokens empty: %+v", got)
	}
	if got.TokenType != "Bearer" {
		t.Errorf("TokenType = %q", got.TokenType)
	}
	if got.AccessExpiresIn != int((15 * time.Minute).Seconds()) {
		t.Errorf("AccessExpiresIn = %d", got.AccessExpiresIn)
	}

	// New access token should parse as access.
	verifier := auth.NewVerifierWithRevocation([]byte(testSecret), revoker)
	if _, err := verifier.ParseAccess(got.AccessToken); err != nil {
		t.Errorf("ParseAccess(new): %v", err)
	}
	if _, err := verifier.ParseRefresh(got.RefreshToken); err != nil {
		t.Errorf("ParseRefresh(new): %v", err)
	}
}

func TestAuth_Refresh_RotatesOldRefreshToken(t *testing.T) {
	ts, issuer, revoker := authTestServer(t)
	refresh, _ := issuer.SignRefresh("matt", "sess-1", nil)

	// First refresh succeeds.
	resp := postJSON(t, ts.URL+"/auth/refresh", map[string]string{
		"Authorization": "Bearer " + refresh,
	}, nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("first refresh status = %d, body = %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// Reusing the same refresh token must fail — it was just rotated.
	resp2 := postJSON(t, ts.URL+"/auth/refresh", map[string]string{
		"Authorization": "Bearer " + refresh,
	}, nil)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replay status = %d, want 401", resp2.StatusCode)
	}

	// Sanity: the old refresh JTI is in the revocation list.
	verifier := auth.NewVerifier([]byte(testSecret))
	c, _ := verifier.Parse(refresh)
	if revoked, _ := revoker.IsRevoked(context.Background(), c.ID); !revoked {
		t.Error("old refresh jti should be revoked after rotation")
	}
}

func TestAuth_Refresh_RejectsAccessToken(t *testing.T) {
	ts, issuer, _ := authTestServer(t)
	access, _ := issuer.SignAccess("matt", "sess-1", nil)

	resp := postJSON(t, ts.URL+"/auth/refresh", map[string]string{
		"Authorization": "Bearer " + access,
	}, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (access used as refresh)", resp.StatusCode)
	}
}

func TestAuth_Refresh_MissingToken(t *testing.T) {
	ts, _, _ := authTestServer(t)
	resp := postJSON(t, ts.URL+"/auth/refresh", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAuth_Refresh_AcceptsJSONBody(t *testing.T) {
	ts, issuer, _ := authTestServer(t)
	refresh, _ := issuer.SignRefresh("matt", "sess-1", nil)

	resp := postJSON(t, ts.URL+"/auth/refresh", nil, map[string]string{
		"refresh_token": refresh,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
}

func TestAuth_Refresh_WrongMethod(t *testing.T) {
	ts, _, _ := authTestServer(t)
	resp, err := http.Get(ts.URL + "/auth/refresh")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

func TestAuth_Revoke_AccessToken_BlocksWSConnect(t *testing.T) {
	ts, issuer, _ := authTestServer(t)
	access, _ := issuer.SignAccess("matt", "sess-1", nil)

	// Pre-revoke: the token works at /ws.
	c, _, err := dialWithAuthHeader(t, ts, access)
	if err != nil {
		t.Fatalf("pre-revoke dial: %v", err)
	}
	_ = c.Close()

	// Revoke the token.
	resp := postJSON(t, ts.URL+"/auth/revoke", map[string]string{
		"Authorization": "Bearer " + access,
	}, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("revoke status = %d, body = %s", resp.StatusCode, body)
	}

	// Post-revoke: the same token is now rejected at /ws.
	_, dialResp, err := dialWithAuthHeader(t, ts, access)
	if err == nil {
		t.Fatal("expected post-revoke dial to fail")
	}
	if dialResp == nil || dialResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %v", dialResp)
	}
}

func TestAuth_Revoke_IsIdempotent(t *testing.T) {
	ts, issuer, _ := authTestServer(t)
	access, _ := issuer.SignAccess("matt", "sess-1", nil)

	first := postJSON(t, ts.URL+"/auth/revoke", map[string]string{
		"Authorization": "Bearer " + access,
	}, nil)
	first.Body.Close()
	if first.StatusCode != http.StatusNoContent {
		t.Fatalf("first revoke = %d", first.StatusCode)
	}
	second := postJSON(t, ts.URL+"/auth/revoke", map[string]string{
		"Authorization": "Bearer " + access,
	}, nil)
	defer second.Body.Close()
	if second.StatusCode != http.StatusNoContent {
		t.Fatalf("second revoke = %d, want 204 (idempotent)", second.StatusCode)
	}
}

func TestAuth_Revoke_RejectsMissingToken(t *testing.T) {
	ts, _, _ := authTestServer(t)
	resp := postJSON(t, ts.URL+"/auth/revoke", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAuth_WSConnect_RejectsRefreshToken(t *testing.T) {
	// /ws must only accept access-kind tokens. Presenting a refresh
	// token (e.g. by mistake or replay) must 401 BEFORE upgrade.
	ts, issuer, _ := authTestServer(t)
	refresh, _ := issuer.SignRefresh("matt", "sess-1", nil)

	_, resp, err := dialWithAuthHeader(t, ts, refresh)
	if err == nil {
		t.Fatal("expected dial to fail with refresh token")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %v", resp)
	}
}

func TestAuth_NoIssuer_NoRefreshEndpoint(t *testing.T) {
	// A server built without the Issuer option should not expose
	// /auth/refresh — confirm with a 404.
	cfg := &config.Config{
		ListenAddr:   "127.0.0.1:0",
		JWTSecret:    []byte(testSecret),
		LogLevel:     slog.LevelInfo,
		Session:      config.SessionConfig{BufferSize: 32, MaxBytes: 1 << 20},
		Sandbox:      config.SandboxConfig{DefaultTimeout: 2 * time.Second, MaxConcurrent: 4},
		Approval:     config.ApprovalConfig{Timeout: 2 * time.Second},
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 2 * time.Second,
		PingInterval: 30 * time.Second,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := hub.New(logger)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go h.Run(ctx)
	sessions := session.NewMemoryStore(32, 1<<20)
	verifier := auth.NewVerifier(cfg.JWTSecret)
	srv := New(cfg, logger, h, sessions, verifier, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp := postJSON(t, ts.URL+"/auth/refresh", nil, nil)
	defer resp.Body.Close()
	// SPA disabled in this cfg → mux falls through with 404.
	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), "page not found") && resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d body = %s; expected 404", resp.StatusCode, body)
		}
	}
}
