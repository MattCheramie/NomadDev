package wsserver

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
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

const testSecret = "test-secret-that-is-definitely-long-enough-32"

func newTestServer(t *testing.T) (*httptest.Server, *Server, session.Store, *auth.IssuerSigner) {
	t.Helper()

	cfg := &config.Config{
		ListenAddr:   "127.0.0.1:0",
		JWTSecret:    []byte(testSecret),
		LogLevel:     slog.LevelInfo,
		Session:      config.SessionConfig{BufferSize: 8, MaxBytes: 1 << 20},
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 2 * time.Second,
		PingInterval: 30 * time.Second,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := hub.New(logger)
	ctx, cancel := context.WithCancel(context.Background())
	go h.Run(ctx)
	t.Cleanup(cancel)

	sessions := session.NewMemoryStore(cfg.Session.BufferSize, cfg.Session.MaxBytes)
	verifier := auth.NewVerifier(cfg.JWTSecret)
	srv := New(cfg, logger, h, sessions, verifier)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	issuer := auth.NewIssuer(cfg.JWTSecret, time.Hour)
	return ts, srv, sessions, issuer
}

func wsURL(ts *httptest.Server, path string) string {
	u, _ := url.Parse(ts.URL)
	u.Scheme = "ws"
	u.Path = path
	return u.String()
}

func dialWithAuthHeader(t *testing.T, ts *httptest.Server, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	return websocket.DefaultDialer.Dial(wsURL(ts, "/ws"), h)
}

func readEnv(t *testing.T, c *websocket.Conn) event.Envelope {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	env, err := event.DecodeBytes(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return env
}

func writeEnv(t *testing.T, c *websocket.Conn, env event.Envelope) {
	t.Helper()
	b, err := env.Bytes()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := c.WriteMessage(websocket.TextMessage, b); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestIntegration_RejectsMissingToken(t *testing.T) {
	ts, _, _, _ := newTestServer(t)
	_, resp, err := websocket.DefaultDialer.Dial(wsURL(ts, "/ws"), nil)
	if err == nil {
		t.Fatal("expected dial to fail without token")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %v", resp)
	}
}

func TestIntegration_RejectsExpiredToken(t *testing.T) {
	ts, _, _, _ := newTestServer(t)
	expired := auth.NewIssuer([]byte(testSecret), -time.Hour)
	tok, _ := expired.Sign("matt", "sess-x", nil)

	_, resp, err := dialWithAuthHeader(t, ts, tok)
	if err == nil {
		t.Fatal("expected dial to fail with expired token")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %v", resp)
	}
}

func TestIntegration_HelloAndPingPong(t *testing.T) {
	ts, _, _, issuer := newTestServer(t)
	tok, _ := issuer.Sign("matt", "sess-1", nil)

	c, _, err := dialWithAuthHeader(t, ts, tok)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	hello := readEnv(t, c)
	if hello.Type != event.EventHello {
		t.Fatalf("first env type = %q, want hello", hello.Type)
	}
	var hp event.HelloPayload
	if err := hello.UnmarshalPayload(&hp); err != nil {
		t.Fatalf("hello payload: %v", err)
	}
	if hp.SessionID != "sess-1" {
		t.Fatalf("hello.session_id = %q", hp.SessionID)
	}

	ping, _ := event.NewEnvelope(event.EventPing, event.PingPayload{Nonce: "n1"})
	writeEnv(t, c, ping)

	pong := readEnv(t, c)
	if pong.Type != event.EventPong {
		t.Fatalf("got %q want pong", pong.Type)
	}
	if pong.CorrelationID != ping.ID {
		t.Fatalf("pong.correlation_id = %q, want %q", pong.CorrelationID, ping.ID)
	}
}

func TestIntegration_SubprotocolNegotiation(t *testing.T) {
	ts, _, _, issuer := newTestServer(t)
	tok, _ := issuer.Sign("matt", "sess-1", nil)

	dialer := *websocket.DefaultDialer
	dialer.Subprotocols = []string{"bearer", tok}
	c, resp, err := dialer.Dial(wsURL(ts, "/ws"), nil)
	if err != nil {
		t.Fatalf("dial: %v (resp=%v)", err, resp)
	}
	defer c.Close()

	if got := c.Subprotocol(); got != "bearer" {
		t.Fatalf("negotiated subprotocol = %q, want bearer", got)
	}
	hello := readEnv(t, c)
	if hello.Type != event.EventHello {
		t.Fatalf("got %q want hello", hello.Type)
	}
}

func TestIntegration_RejectsUnknownEventType(t *testing.T) {
	ts, _, _, issuer := newTestServer(t)
	tok, _ := issuer.Sign("matt", "sess-1", nil)
	c, _, err := dialWithAuthHeader(t, ts, tok)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = readEnv(t, c) // hello

	weird, _ := event.NewEnvelope("totally.made.up", nil)
	writeEnv(t, c, weird)

	got := readEnv(t, c)
	if got.Type != event.EventError {
		t.Fatalf("got %q want error", got.Type)
	}
	var p event.ErrorPayload
	_ = got.UnmarshalPayload(&p)
	if p.Code != event.CodeUnknownType {
		t.Fatalf("error.code = %q want %q", p.Code, event.CodeUnknownType)
	}
}

func TestIntegration_CommandRequest_NotImplemented(t *testing.T) {
	ts, _, _, issuer := newTestServer(t)
	tok, _ := issuer.Sign("matt", "sess-1", nil)
	c, _, err := dialWithAuthHeader(t, ts, tok)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = readEnv(t, c) // hello

	req, _ := event.NewEnvelope(event.EventCommandRequest, map[string]any{"tool": "noop"})
	writeEnv(t, c, req)

	got := readEnv(t, c)
	if got.Type != event.EventError {
		t.Fatalf("got %q want error", got.Type)
	}
	var p event.ErrorPayload
	_ = got.UnmarshalPayload(&p)
	if p.Code != event.CodeNotImplemented {
		t.Fatalf("error.code = %q want %q", p.Code, event.CodeNotImplemented)
	}
}

func TestIntegration_Reconnect_ReplaysMissedEvents(t *testing.T) {
	ts, _, sessions, issuer := newTestServer(t)
	tok, _ := issuer.Sign("matt", "sess-1", nil)

	c1, _, err := dialWithAuthHeader(t, ts, tok)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	hello := readEnv(t, c1)
	if hello.Type != event.EventHello {
		t.Fatalf("first env = %q", hello.Type)
	}

	// Use ping/pong to mint replayable events with predictable ids.
	ping1, _ := event.NewEnvelope(event.EventPing, event.PingPayload{Nonce: "p1"})
	writeEnv(t, c1, ping1)
	pong1 := readEnv(t, c1)
	if pong1.Type != event.EventPong {
		t.Fatalf("expected pong, got %q", pong1.Type)
	}

	// Disconnect WITHOUT reading the next pong.
	ping2, _ := event.NewEnvelope(event.EventPing, event.PingPayload{Nonce: "p2"})
	writeEnv(t, c1, ping2)
	// Give the server a moment to enqueue + buffer the pong.
	time.Sleep(50 * time.Millisecond)
	_ = c1.Close()

	// Wait for the server to fully tear down the old connection before
	// reconnecting so the hub doesn't immediately replace ourselves.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		sess := sessions.Get("sess-1")
		_, last := sess.BufferBounds()
		if last >= pong1.ID {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)

	c2, _, err := dialWithAuthHeader(t, ts, tok)
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	defer c2.Close()

	hello2 := readEnv(t, c2)
	if hello2.Type != event.EventHello {
		t.Fatalf("reconnect first env = %q", hello2.Type)
	}

	clientHello, _ := event.NewEnvelope(event.EventClientHello, event.ClientHelloPayload{
		LastEventID: pong1.ID,
	})
	writeEnv(t, c2, clientHello)

	// We expect the missed pong (response to ping2) to be replayed.
	var replayed []event.Envelope
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_ = c2.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, data, err := c2.ReadMessage()
		if err != nil {
			break
		}
		env, err := event.DecodeBytes(data)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		replayed = append(replayed, env)
		if env.Type == event.EventPong && env.CorrelationID == ping2.ID {
			break
		}
	}
	found := false
	for _, e := range replayed {
		if e.Type == event.EventPong && e.CorrelationID == ping2.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected to replay pong for ping2 %q; got %d envelopes", ping2.ID, len(replayed))
	}
}

func TestIntegration_Reconnect_StaleSession(t *testing.T) {
	ts, _, _, issuer := newTestServer(t)
	tok, _ := issuer.Sign("matt", "sess-stale", nil)

	c, _, err := dialWithAuthHeader(t, ts, tok)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = readEnv(t, c) // hello

	// Send a client.hello with a phantom last_event_id that the server has
	// never minted. The buffer contains only the `hello` envelope.
	bogus := strings.Repeat("0", 26) // valid ULID length, sorts before anything real
	ch, _ := event.NewEnvelope(event.EventClientHello, event.ClientHelloPayload{
		LastEventID: bogus,
	})
	writeEnv(t, c, ch)

	got := readEnv(t, c)
	if got.Type != event.EventSessionStale {
		t.Fatalf("got %q want session.stale", got.Type)
	}
}
