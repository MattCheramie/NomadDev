// Command wsclient is a tiny WebSocket client for human smoke tests and
// scripted verification of the orchestrator.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/mattcheramie/nomaddev/internal/event"
	"github.com/mattcheramie/nomaddev/internal/wireclient"
)

const reconnectAuto = "auto"

func main() {
	url := flag.String("url", "ws://127.0.0.1:8080/ws", "orchestrator ws URL")
	token := flag.String("token", "", "JWT bearer token")
	send := flag.String("send", "", "envelope type to send after hello (e.g. ping, command.request)")
	subprotocol := flag.Bool("subprotocol", false, "send token via Sec-WebSocket-Protocol instead of Authorization")
	timeout := flag.Duration("timeout", 5*time.Second, "read timeout for each frame")
	disconnectAfter := flag.String("disconnect-after", "",
		"close the connection after observing an envelope of this type")
	reconnectWith := flag.String("reconnect-with-last-event-id", "",
		`after disconnect, reconnect and send client.hello with the given last_event_id ("auto" = last id observed)`)
	script := flag.String("script", "", "with -send command.request: the script body")
	shell := flag.String("shell", "bash", "with -send command.request: shell to run the script")
	cmdTimeout := flag.Int("cmd-timeout-ms", 5000, "with -send command.request: timeout_ms on the request payload")
	text := flag.String("text", "", "with -send user.intent: the free-text turn body")
	correlationID := flag.String("correlation-id", "", "explicit correlation_id (overrides the default)")
	denyReason := flag.String("deny-reason", "", "with -send tool.approval.denied: optional reason")
	flag.Parse()

	if *token == "" {
		fmt.Fprintln(os.Stderr, "wsclient: -token is required")
		os.Exit(2)
	}

	payload, err := buildPayload(*send, *shell, *script, *cmdTimeout, *text, *denyReason)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wsclient:", err)
		os.Exit(2)
	}

	opts := wireclient.DialOptions{URL: *url, Token: *token, UseSubprotocol: *subprotocol}

	lastID, err := runSession(opts, *send, payload, *correlationID, *disconnectAfter, *timeout)
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
	if err := resume(opts, resumeID, *timeout); err != nil {
		fmt.Fprintln(os.Stderr, "wsclient: resume:", err)
		os.Exit(1)
	}
}

// buildPayload returns the typed payload for the requested -send type, or nil
// if the envelope carries no payload by convention.
func buildPayload(send, shell, script string, cmdTimeoutMs int, text, denyReason string) (any, error) {
	switch send {
	case "", event.EventClientHello:
		return nil, nil
	case event.EventPing:
		return event.PingPayload{Nonce: "wsclient"}, nil
	case event.EventUserIntent:
		if text == "" {
			return nil, fmt.Errorf("-text is required with -send user.intent")
		}
		return event.UserIntentPayload{Text: text}, nil
	case event.EventToolApprovalGranted:
		return event.ToolApprovalGrantedPayload{}, nil
	case event.EventToolApprovalDenied:
		return event.ToolApprovalDeniedPayload{Reason: denyReason}, nil
	case event.EventCommandRequest:
		if script == "" {
			return nil, fmt.Errorf("-script is required with -send command.request")
		}
		return event.CommandRequestPayload{
			Tool:      "execute_script",
			Args:      map[string]any{"shell": shell, "script": script},
			TimeoutMs: cmdTimeoutMs,
		}, nil
	default:
		return nil, nil
	}
}

// runSession dials, reads the hello, optionally sends one envelope, optionally
// disconnects on a given reply type. Returns the last envelope id observed.
// correlationID, when non-empty, stamps the outgoing envelope so the client
// can target a specific request — useful for sending tool.approval.granted
// in response to a tool.approval.request the operator just observed.
func runSession(
	opts wireclient.DialOptions,
	send string,
	payload any,
	correlationID, disconnectAfter string,
	timeout time.Duration,
) (string, error) {
	conn, err := wireclient.Dial(opts)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	hello, helloBytes, err := conn.ReadEnvelopeRaw(timeout)
	if err != nil {
		return "", fmt.Errorf("read hello: %w", err)
	}
	fmt.Printf("<- %s\n", helloBytes)
	lastID := hello.ID

	if send == "" {
		return lastID, nil
	}

	env, err := event.NewEnvelope(send, payload)
	if err != nil {
		return lastID, fmt.Errorf("build envelope: %w", err)
	}
	if correlationID != "" {
		env.CorrelationID = correlationID
	}
	b, err := conn.WriteEnvelopeBytes(env)
	if err != nil {
		return lastID, err
	}
	fmt.Printf("-> %s\n", b)

	// If no disconnect-after, read exactly one reply.
	if disconnectAfter == "" {
		got, gotBytes, err := conn.ReadEnvelopeRaw(timeout)
		if err != nil {
			return lastID, fmt.Errorf("read reply: %w", err)
		}
		fmt.Printf("<- %s\n", gotBytes)
		return got.ID, nil
	}

	for {
		got, gotBytes, err := conn.ReadEnvelopeRaw(timeout)
		if err != nil {
			return lastID, fmt.Errorf("read: %w", err)
		}
		fmt.Printf("<- %s\n", gotBytes)
		lastID = got.ID
		if got.Type == disconnectAfter {
			return lastID, nil
		}
	}
}

// resume reconnects, sends a client.hello with lastID, and prints whatever the
// server replays (until the read deadline elapses).
func resume(opts wireclient.DialOptions, lastID string, timeout time.Duration) error {
	conn, err := wireclient.Dial(opts)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, helloBytes, err := conn.ReadEnvelopeRaw(timeout); err != nil {
		return fmt.Errorf("read hello: %w", err)
	} else {
		fmt.Printf("<- %s\n", helloBytes)
	}

	clientHello, err := event.NewEnvelope(event.EventClientHello, event.ClientHelloPayload{LastEventID: lastID})
	if err != nil {
		return fmt.Errorf("build client.hello: %w", err)
	}
	b, err := conn.WriteEnvelopeBytes(clientHello)
	if err != nil {
		return fmt.Errorf("write client.hello: %w", err)
	}
	fmt.Printf("-> %s\n", b)

	// Drain replayed events until the read deadline trips.
	for {
		_, gotBytes, err := conn.ReadEnvelopeRaw(timeout)
		if err != nil {
			return nil
		}
		fmt.Printf("<- %s\n", gotBytes)
	}
}
