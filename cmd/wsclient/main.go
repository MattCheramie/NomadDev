// Command wsclient is a tiny WebSocket client for human smoke tests and
// scripted verification of the orchestrator.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/gorilla/websocket"

	"github.com/mattcheramie/nomaddev/internal/event"
)

const reconnectAuto = "auto"

func main() {
	url := flag.String("url", "ws://127.0.0.1:8080/ws", "orchestrator ws URL")
	token := flag.String("token", "", "JWT bearer token")
	send := flag.String("send", "", "envelope type to send after hello (e.g. ping)")
	subprotocol := flag.Bool("subprotocol", false, "send token via Sec-WebSocket-Protocol instead of Authorization")
	timeout := flag.Duration("timeout", 5*time.Second, "read timeout for each frame")
	disconnectAfter := flag.String("disconnect-after", "",
		"close the connection after observing an envelope of this type")
	reconnectWith := flag.String("reconnect-with-last-event-id", "",
		`after disconnect, reconnect and send client.hello with the given last_event_id ("auto" = last id observed)`)
	flag.Parse()

	if *token == "" {
		fmt.Fprintln(os.Stderr, "wsclient: -token is required")
		os.Exit(2)
	}

	dialer := *websocket.DefaultDialer
	var hdr http.Header
	if *subprotocol {
		dialer.Subprotocols = []string{"bearer", *token}
	} else {
		hdr = http.Header{}
		hdr.Set("Authorization", "Bearer "+*token)
	}

	lastID, err := runSession(&dialer, hdr, *url, *send, *disconnectAfter, *timeout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wsclient:", err)
		os.Exit(1)
	}

	if *reconnectWith == "" {
		return
	}
	resumeID := *reconnectWith
	if resumeID == reconnectAuto {
		if lastID == "" {
			fmt.Fprintln(os.Stderr, "wsclient: no events observed, cannot auto-resume")
			os.Exit(1)
		}
		resumeID = lastID
	}
	fmt.Printf("== reconnecting with last_event_id=%s ==\n", resumeID)
	if err := resume(&dialer, hdr, *url, resumeID, *timeout); err != nil {
		fmt.Fprintln(os.Stderr, "wsclient: resume:", err)
		os.Exit(1)
	}
}

// runSession dials, reads the hello, optionally sends one envelope, optionally
// disconnects on a given reply type. Returns the last envelope id observed.
func runSession(
	d *websocket.Dialer,
	hdr http.Header,
	url, send, disconnectAfter string,
	timeout time.Duration,
) (string, error) {
	conn, resp, err := d.Dial(url, hdr)
	if err != nil {
		if resp != nil {
			return "", fmt.Errorf("dial: %w (status=%d)", err, resp.StatusCode)
		}
		return "", fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	hello, err := readOne(conn, timeout)
	if err != nil {
		return "", fmt.Errorf("read hello: %w", err)
	}
	lastID := hello.ID

	if send == "" {
		return lastID, nil
	}

	var payload any
	if send == event.EventPing {
		payload = event.PingPayload{Nonce: "wsclient"}
	}
	env, err := event.NewEnvelope(send, payload)
	if err != nil {
		return lastID, fmt.Errorf("build envelope: %w", err)
	}
	b, _ := env.Bytes()
	if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
		return lastID, fmt.Errorf("write: %w", err)
	}
	fmt.Printf("-> %s\n", b)

	// If no disconnect-after, read exactly one reply.
	if disconnectAfter == "" {
		got, err := readOne(conn, timeout)
		if err != nil {
			return lastID, fmt.Errorf("read reply: %w", err)
		}
		return got.ID, nil
	}

	for {
		got, err := readOne(conn, timeout)
		if err != nil {
			return lastID, fmt.Errorf("read: %w", err)
		}
		lastID = got.ID
		if got.Type == disconnectAfter {
			return lastID, nil
		}
	}
}

// resume reconnects, sends a client.hello with lastID, and prints whatever the
// server replays (until the read deadline elapses).
func resume(d *websocket.Dialer, hdr http.Header, url, lastID string, timeout time.Duration) error {
	conn, resp, err := d.Dial(url, hdr)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("dial: %w (status=%d)", err, resp.StatusCode)
		}
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	if _, err := readOne(conn, timeout); err != nil {
		return fmt.Errorf("read hello: %w", err)
	}

	clientHello, err := event.NewEnvelope(event.EventClientHello, event.ClientHelloPayload{LastEventID: lastID})
	if err != nil {
		return fmt.Errorf("build client.hello: %w", err)
	}
	b, _ := clientHello.Bytes()
	if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
		return fmt.Errorf("write client.hello: %w", err)
	}
	fmt.Printf("-> %s\n", b)

	// Drain replayed events until the read deadline trips.
	for {
		if _, err := readOne(conn, timeout); err != nil {
			return nil
		}
	}
}

func readOne(c *websocket.Conn, timeout time.Duration) (event.Envelope, error) {
	_ = c.SetReadDeadline(time.Now().Add(timeout))
	_, data, err := c.ReadMessage()
	if err != nil {
		return event.Envelope{}, err
	}
	fmt.Printf("<- %s\n", data)
	return event.DecodeBytes(data)
}
