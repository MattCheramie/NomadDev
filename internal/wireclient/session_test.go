package wireclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/mattcheramie/nomaddev/internal/event"
)

// sessionFakeServer accepts a series of WebSocket connections and lets the
// test drive each one — send hellos, capture inbound frames, close at will.
type sessionFakeServer struct {
	mu        sync.Mutex
	conns     []*websocket.Conn
	got       chan event.Envelope
	upgrader  websocket.Upgrader
	respondFn func(*websocket.Conn)
	dialCount atomic.Int32
}

func newSessionFakeServer(t *testing.T, respond func(*websocket.Conn)) (*httptest.Server, *sessionFakeServer) {
	t.Helper()
	f := &sessionFakeServer{
		got: make(chan event.Envelope, 16),
		upgrader: websocket.Upgrader{
			CheckOrigin:  func(r *http.Request) bool { return true },
			Subprotocols: []string{"bearer"},
		},
		respondFn: respond,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.dialCount.Add(1)
		conn, err := f.upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conns = append(f.conns, conn)
		f.mu.Unlock()
		f.respondFn(conn)
	}))
	return srv, f
}

func wsURLFrom(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http") + "/ws"
}

func TestSession_StatusTransitionsAndHelloObserved(t *testing.T) {
	respond := func(c *websocket.Conn) {
		env, _ := event.NewEnvelope(event.EventHello, event.HelloPayload{SessionID: "sess-1"})
		b, _ := env.Bytes()
		_ = c.WriteMessage(websocket.TextMessage, b)
		// keep open until the test closes us
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}
	srv, _ := newSessionFakeServer(t, respond)
	defer srv.Close()

	var (
		mu       sync.Mutex
		statuses []Status
		envs     []event.Envelope
	)
	sess := NewSession(SessionConfig{
		Dial: DialOptions{URL: wsURLFrom(srv.URL), Token: "t"},
		OnStatus: func(s Status) {
			mu.Lock()
			statuses = append(statuses, s)
			mu.Unlock()
		},
		OnEnvelope: func(env event.Envelope) {
			mu.Lock()
			envs = append(envs, env)
			mu.Unlock()
		},
		ReconnectBase: 10 * time.Millisecond,
		ReconnectCap:  20 * time.Millisecond,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = sess.Run(ctx)
		close(done)
	}()

	// Wait until we're open.
	waitFor(t, time.Second, func() bool { return sess.Status() == StatusOpen })

	sess.Close()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(statuses) < 2 {
		t.Fatalf("want at least connecting+open, got %v", statuses)
	}
	if statuses[0] != StatusConnecting {
		t.Fatalf("first status = %q, want connecting", statuses[0])
	}
	foundOpen := false
	for _, s := range statuses {
		if s == StatusOpen {
			foundOpen = true
		}
	}
	if !foundOpen {
		t.Fatalf("never saw StatusOpen; got %v", statuses)
	}
	if len(envs) == 0 || envs[0].Type != event.EventHello {
		t.Fatalf("first envelope = %+v, want hello", envs)
	}
	if sess.LastEventID() == "" {
		t.Fatal("LastEventID should be set after observing hello")
	}
}

func TestSession_Send_QueuesUserIntentWhileClosed(t *testing.T) {
	sess := NewSession(SessionConfig{Dial: DialOptions{URL: "ws://nope/ws", Token: "t"}})
	// Session has never run; statusVal is StatusIdle, conn is nil. user.intent
	// should go to the outbox; ping should be rejected.
	intent, _ := event.NewEnvelope(event.EventUserIntent, event.UserIntentPayload{Text: "hi"})
	if err := sess.Send(intent); err != nil {
		t.Fatalf("queue user.intent: %v", err)
	}
	if sess.OutboxLen() != 1 {
		t.Fatalf("outbox len = %d, want 1", sess.OutboxLen())
	}
	ping, _ := event.NewEnvelope(event.EventPing, event.PingPayload{})
	if err := sess.Send(ping); err != ErrOffline {
		t.Fatalf("ping while offline: err = %v, want ErrOffline", err)
	}
}

func TestSession_Send_OutboxCapDropsOldest(t *testing.T) {
	sess := NewSession(SessionConfig{
		Dial:      DialOptions{URL: "ws://nope/ws", Token: "t"},
		OutboxCap: 3,
	})
	for i := 0; i < 5; i++ {
		env, _ := event.NewEnvelope(event.EventUserIntent, event.UserIntentPayload{Text: "t"})
		// stamp the payload so we can identify the survivors
		env.Payload = json.RawMessage([]byte(`{"text":"` + string(rune('a'+i)) + `"}`))
		_ = sess.Send(env)
	}
	if got := sess.OutboxLen(); got != 3 {
		t.Fatalf("outbox len = %d, want 3 (cap)", got)
	}
	// The three survivors should be c, d, e (oldest two dropped).
	sess.mu.Lock()
	defer sess.mu.Unlock()
	var texts []string
	for _, env := range sess.outbox {
		var p event.UserIntentPayload
		_ = json.Unmarshal(env.Payload, &p)
		texts = append(texts, p.Text)
	}
	want := []string{"c", "d", "e"}
	for i := range want {
		if texts[i] != want[i] {
			t.Fatalf("outbox[%d] = %q, want %q (full: %v)", i, texts[i], want[i], texts)
		}
	}
}

func TestSession_DrainsOutboxOnReconnect(t *testing.T) {
	respond := func(c *websocket.Conn) {
		env, _ := event.NewEnvelope(event.EventHello, event.HelloPayload{SessionID: "sess"})
		b, _ := env.Bytes()
		_ = c.WriteMessage(websocket.TextMessage, b)
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			ev, _ := event.DecodeBytes(data)
			// echo back so the test can observe drain
			ack, _ := event.NewReply(event.EventAck, ev.ID, event.AckPayload{Action: "echo"})
			ab, _ := ack.Bytes()
			_ = c.WriteMessage(websocket.TextMessage, ab)
		}
	}
	srv, _ := newSessionFakeServer(t, respond)
	defer srv.Close()

	echoes := make(chan event.Envelope, 4)
	sess := NewSession(SessionConfig{
		Dial: DialOptions{URL: wsURLFrom(srv.URL), Token: "t"},
		OnEnvelope: func(env event.Envelope) {
			if env.Type == event.EventAck {
				echoes <- env
			}
		},
		ReconnectBase: 10 * time.Millisecond,
		ReconnectCap:  20 * time.Millisecond,
	})

	// Queue two intents before Run.
	for _, txt := range []string{"one", "two"} {
		env, _ := event.NewEnvelope(event.EventUserIntent, event.UserIntentPayload{Text: txt})
		_ = sess.Send(env)
	}
	if sess.OutboxLen() != 2 {
		t.Fatalf("outbox len = %d, want 2", sess.OutboxLen())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = sess.Run(ctx)
		close(done)
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-echoes:
		case <-time.After(time.Second):
			t.Fatalf("did not receive echo %d", i+1)
		}
	}
	if sess.OutboxLen() != 0 {
		t.Fatalf("outbox should be drained, len = %d", sess.OutboxLen())
	}

	sess.Close()
	<-done
}

func TestSession_StopsOnUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	defer srv.Close()

	var lastStatus Status
	var statusMu sync.Mutex
	sess := NewSession(SessionConfig{
		Dial:          DialOptions{URL: wsURLFrom(srv.URL), Token: "bad"},
		ReconnectBase: 10 * time.Millisecond,
		ReconnectCap:  20 * time.Millisecond,
		OnStatus: func(s Status) {
			statusMu.Lock()
			lastStatus = s
			statusMu.Unlock()
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := sess.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	statusMu.Lock()
	defer statusMu.Unlock()
	if lastStatus != StatusUnauthorized {
		t.Fatalf("final status = %q, want unauthorized", lastStatus)
	}
}

func TestNextBackoffSaturatesAtCap(t *testing.T) {
	got := nextBackoff(20*time.Second, 30*time.Second)
	if got != 30*time.Second {
		t.Fatalf("nextBackoff(20s,30s) = %v, want 30s", got)
	}
	got = nextBackoff(4*time.Second, 30*time.Second)
	if got != 8*time.Second {
		t.Fatalf("nextBackoff(4s,30s) = %v, want 8s", got)
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", d)
}
