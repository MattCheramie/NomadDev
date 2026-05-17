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

func main() {
	url := flag.String("url", "ws://127.0.0.1:8080/ws", "orchestrator ws URL")
	token := flag.String("token", "", "JWT bearer token")
	send := flag.String("send", "", "envelope type to send after hello (e.g. ping)")
	subprotocol := flag.Bool("subprotocol", false, "send token via Sec-WebSocket-Protocol instead of Authorization")
	timeout := flag.Duration("timeout", 5*time.Second, "read timeout for each frame")
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

	conn, resp, err := dialer.Dial(*url, hdr)
	if err != nil {
		if resp != nil {
			fmt.Fprintf(os.Stderr, "wsclient: dial: %v (status=%d)\n", err, resp.StatusCode)
		} else {
			fmt.Fprintf(os.Stderr, "wsclient: dial: %v\n", err)
		}
		os.Exit(1)
	}
	defer conn.Close()

	if err := readOne(conn, *timeout); err != nil {
		fmt.Fprintf(os.Stderr, "wsclient: read hello: %v\n", err)
		os.Exit(1)
	}

	if *send == "" {
		return
	}

	var payload any
	switch *send {
	case event.EventPing:
		payload = event.PingPayload{Nonce: "wsclient"}
	default:
	}
	env, err := event.NewEnvelope(*send, payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wsclient: build envelope: %v\n", err)
		os.Exit(1)
	}
	b, _ := env.Bytes()
	if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
		fmt.Fprintf(os.Stderr, "wsclient: write: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("-> %s\n", b)

	if err := readOne(conn, *timeout); err != nil {
		fmt.Fprintf(os.Stderr, "wsclient: read reply: %v\n", err)
		os.Exit(1)
	}
}

func readOne(c *websocket.Conn, timeout time.Duration) error {
	_ = c.SetReadDeadline(time.Now().Add(timeout))
	_, data, err := c.ReadMessage()
	if err != nil {
		return err
	}
	fmt.Printf("<- %s\n", data)
	return nil
}
