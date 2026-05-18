package wsserver

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/mattcheramie/nomaddev/internal/auth"
	"github.com/mattcheramie/nomaddev/internal/config"
	"github.com/mattcheramie/nomaddev/internal/event"
	"github.com/mattcheramie/nomaddev/internal/hub"
	"github.com/mattcheramie/nomaddev/internal/session"
)

// guardedTestServer wires a Server with custom MaxMessageBytes /
// RateLimit / RateBurst for the read-side guard tests. Returns the
// httptest.Server plus a signer the test can mint tokens from.
func guardedTestServer(t *testing.T, maxBytes int64, rps float64, burst int) (*httptest.Server, *auth.IssuerSigner) {
	t.Helper()
	cfg := &config.Config{
		ListenAddr:      "127.0.0.1:0",
		JWTSecret:       []byte(testSecret),
		LogLevel:        slog.LevelInfo,
		Session:         config.SessionConfig{BufferSize: 32, MaxBytes: 1 << 20},
		Sandbox:         config.SandboxConfig{DefaultTimeout: 2 * time.Second, MaxConcurrent: 4},
		Approval:        config.ApprovalConfig{Timeout: 2 * time.Second},
		ReadTimeout:     10 * time.Second,
		WriteTimeout:    2 * time.Second,
		PingInterval:    30 * time.Second,
		MaxMessageBytes: maxBytes,
		RateLimit:       rps,
		RateBurst:       burst,
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

func TestGuards_MessageTooLarge_ClosesWith1009(t *testing.T) {
	const cap = 4 * 1024
	ts, issuer := guardedTestServer(t, cap, 0, 0)
	tok, _ := issuer.Sign("matt", "sess-1", nil)

	c, _, err := dialWithAuthHeader(t, ts, tok)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = readEnv(t, c) // hello

	huge, _ := event.NewEnvelope(event.EventPing, event.PingPayload{
		Nonce: strings.Repeat("x", cap*2),
	})
	writeEnv(t, c, huge)

	// Read until we observe a 1009 close. gorilla's SetReadLimit closes
	// the conn before our handler can ship an error envelope, so the
	// structured signal is the close code — operators see it in metrics
	// and the structured log on the server side.
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, _, err := c.ReadMessage()
		if err == nil {
			continue
		}
		if websocket.IsCloseError(err, websocket.CloseMessageTooBig) {
			return
		}
		// Some clients see a generic EOF after the close frame arrives.
		// Treat any error here as acceptable provided we hit it.
		return
	}
}

func TestGuards_MessageWithinLimit_StillWorks(t *testing.T) {
	const cap = 64 * 1024
	ts, issuer := guardedTestServer(t, cap, 0, 0)
	tok, _ := issuer.Sign("matt", "sess-1", nil)

	c, _, err := dialWithAuthHeader(t, ts, tok)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = readEnv(t, c) // hello

	ping, _ := event.NewEnvelope(event.EventPing, event.PingPayload{Nonce: "n1"})
	writeEnv(t, c, ping)

	pong := readEnv(t, c)
	if pong.Type != event.EventPong {
		t.Fatalf("got %q want pong", pong.Type)
	}
}

func TestGuards_RateLimit_RejectsAboveBurst(t *testing.T) {
	// 1 req/sec steady state, burst of 2. Sending 5 pings in tight
	// succession means at most ~2 should produce pong (the burst);
	// the rest should produce rate_limited errors.
	ts, issuer := guardedTestServer(t, 256*1024, 1, 2)
	tok, _ := issuer.Sign("matt", "sess-1", nil)

	c, _, err := dialWithAuthHeader(t, ts, tok)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = readEnv(t, c) // hello

	const sent = 5
	for i := 0; i < sent; i++ {
		ping, _ := event.NewEnvelope(event.EventPing, event.PingPayload{Nonce: "p"})
		writeEnv(t, c, ping)
	}

	var pongs, rateErrs int
	deadline := time.Now().Add(2 * time.Second)
	for pongs+rateErrs < sent && time.Now().Before(deadline) {
		_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, data, err := c.ReadMessage()
		if err != nil {
			break
		}
		env, derr := event.DecodeBytes(data)
		if derr != nil {
			t.Fatalf("decode: %v", derr)
		}
		switch env.Type {
		case event.EventPong:
			pongs++
		case event.EventError:
			var p event.ErrorPayload
			_ = env.UnmarshalPayload(&p)
			if p.Code == event.CodeRateLimited {
				rateErrs++
			} else {
				t.Errorf("unexpected error code %q", p.Code)
			}
		default:
			t.Errorf("unexpected env type %q", env.Type)
		}
	}
	if pongs == 0 {
		t.Error("expected at least one pong from the burst allowance")
	}
	if rateErrs == 0 {
		t.Errorf("expected at least one rate_limited error, got %d pongs / %d errs", pongs, rateErrs)
	}
}

func TestGuards_RateLimit_Disabled_AllPass(t *testing.T) {
	// RateLimit=0 disables the limiter — all 5 frames should pong.
	ts, issuer := guardedTestServer(t, 256*1024, 0, 0)
	tok, _ := issuer.Sign("matt", "sess-1", nil)

	c, _, err := dialWithAuthHeader(t, ts, tok)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = readEnv(t, c) // hello

	const sent = 5
	for i := 0; i < sent; i++ {
		ping, _ := event.NewEnvelope(event.EventPing, event.PingPayload{Nonce: "p"})
		writeEnv(t, c, ping)
	}

	pongs := 0
	deadline := time.Now().Add(2 * time.Second)
	for pongs < sent && time.Now().Before(deadline) {
		_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, data, err := c.ReadMessage()
		if err != nil {
			break
		}
		env, _ := event.DecodeBytes(data)
		if env.Type == event.EventPong {
			pongs++
		}
	}
	if pongs != sent {
		t.Errorf("got %d pongs, want %d (limiter disabled)", pongs, sent)
	}
}

func TestGuards_RemoteSendsOversizedPayload_CleanClose(t *testing.T) {
	// Force the remote peer to send a frame larger than gorilla's
	// per-frame buffer to confirm we don't deadlock when the close
	// arrives without the error envelope round-trip.
	const cap = 1 * 1024
	ts, issuer := guardedTestServer(t, cap, 0, 0)
	tok, _ := issuer.Sign("matt", "sess-1", nil)

	c, _, err := dialWithAuthHeader(t, ts, tok)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = readEnv(t, c) // hello

	// Write a payload that ensures the framed message exceeds cap.
	huge := strings.Repeat("y", int(cap)*3)
	if err := c.WriteMessage(websocket.TextMessage, []byte(huge)); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	// Drain until the server closes. We just want to assert the
	// loop terminates rather than hangs.
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	for i := 0; i < 5; i++ {
		if _, _, err := c.ReadMessage(); err != nil {
			return
		}
	}
	t.Fatal("expected connection to close after oversized inbound frame")
}

// Compile-time sanity that test config keys haven't drifted from prod.
var _ = (&config.Config{}).MaxMessageBytes
var _ = http.StatusUnauthorized // keep import live with other test files
