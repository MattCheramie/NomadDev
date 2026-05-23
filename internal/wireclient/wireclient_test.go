package wireclient

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/mattcheramie/nomaddev/internal/event"
)

// fakeServer accepts one WebSocket connection, captures the auth path the
// client used (header vs subprotocol), and echoes one hello back. It's the
// minimal harness for asserting that Dial / Read / Write actually move bytes.
type fakeServer struct {
	upgrader     websocket.Upgrader
	authHeader   string
	subprotocols []string
	got          chan event.Envelope
}

func newFakeServer(t *testing.T) (*httptest.Server, *fakeServer) {
	t.Helper()
	f := &fakeServer{
		upgrader: websocket.Upgrader{
			CheckOrigin:  func(r *http.Request) bool { return true },
			Subprotocols: []string{"bearer"},
		},
		got: make(chan event.Envelope, 4),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.authHeader = r.Header.Get("Authorization")
		f.subprotocols = websocket.Subprotocols(r)
		conn, err := f.upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		hello, _ := event.NewEnvelope(event.EventHello, event.HelloPayload{SessionID: "sess-test"})
		b, _ := hello.Bytes()
		_ = conn.WriteMessage(websocket.TextMessage, b)
		// Drain at most one incoming frame so WriteEnvelope round-trips.
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		_, data, err := conn.ReadMessage()
		if err == nil {
			env, _ := event.DecodeBytes(data)
			f.got <- env
		}
	}))
	return srv, f
}

func wsURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http")
}

func TestDial_AuthorizationHeader(t *testing.T) {
	srv, fake := newFakeServer(t)
	defer srv.Close()
	conn, err := Dial(DialOptions{URL: wsURL(srv.URL) + "/ws", Token: "tok-123"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ReadEnvelope(time.Second); err != nil {
		t.Fatalf("read hello: %v", err)
	}
	if fake.authHeader != "Bearer tok-123" {
		t.Fatalf("want Bearer header, got %q", fake.authHeader)
	}
}

func TestDial_Subprotocol(t *testing.T) {
	srv, fake := newFakeServer(t)
	defer srv.Close()
	conn, err := Dial(DialOptions{URL: wsURL(srv.URL) + "/ws", Token: "tok-456", UseSubprotocol: true})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ReadEnvelope(time.Second); err != nil {
		t.Fatalf("read hello: %v", err)
	}
	if fake.authHeader != "" {
		t.Fatalf("Authorization header should be empty in subprotocol mode, got %q", fake.authHeader)
	}
	want := []string{"bearer", "tok-456"}
	if len(fake.subprotocols) != 2 || fake.subprotocols[0] != want[0] || fake.subprotocols[1] != want[1] {
		t.Fatalf("subprotocols = %v, want %v", fake.subprotocols, want)
	}
}

func TestWriteEnvelope_RoundTrip(t *testing.T) {
	srv, fake := newFakeServer(t)
	defer srv.Close()
	conn, err := Dial(DialOptions{URL: wsURL(srv.URL) + "/ws", Token: "t"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ReadEnvelope(time.Second); err != nil {
		t.Fatalf("read hello: %v", err)
	}
	ping, _ := event.NewEnvelope(event.EventPing, event.PingPayload{Nonce: "n1"})
	if err := conn.WriteEnvelope(ping); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case got := <-fake.got:
		if got.Type != event.EventPing {
			t.Fatalf("server got type %q, want ping", got.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("server never received ping")
	}
}

func TestDial_RequiresURLAndToken(t *testing.T) {
	if _, err := Dial(DialOptions{Token: "t"}); err == nil {
		t.Fatal("want error for missing URL")
	}
	if _, err := Dial(DialOptions{URL: "ws://x/ws"}); err == nil {
		t.Fatal("want error for missing token")
	}
}
