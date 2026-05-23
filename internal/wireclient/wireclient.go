// Package wireclient is a thin, transport-level WebSocket client for the
// NomadDev orchestrator. It hides gorilla/websocket behind an envelope-aware
// Conn so the CLI smoke tool (cmd/wsclient) and the native Gio mobile app
// (cmd/nomaddev-mobile) speak the same protocol the same way.
//
// This package owns dialing, single-frame read/write, and auth header
// composition. Higher-level concerns — reconnect, outbox, replay, dispatch
// to UI — live one layer up so they can evolve without touching transport.
package wireclient

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"github.com/mattcheramie/nomaddev/internal/event"
)

// DialOptions configures a single Dial. The orchestrator accepts the JWT
// either in the Authorization header or, when phones are constrained by
// proxies that strip Authorization on Upgrade, in the Sec-WebSocket-Protocol
// subprotocol list as "bearer, <token>".
type DialOptions struct {
	URL            string
	Token          string
	UseSubprotocol bool
}

// Conn is an envelope-level wrapper around *websocket.Conn. The mobile app
// must not import gorilla/websocket directly — every protocol concern is
// owned by this package and internal/event.
type Conn struct {
	inner *websocket.Conn
}

// Dial opens one WebSocket connection. On success the caller owns the Conn
// and must Close it. On failure the HTTP status code, if any, is included in
// the error so the caller can distinguish 401/403 from network errors.
func Dial(opts DialOptions) (*Conn, error) {
	if opts.URL == "" {
		return nil, fmt.Errorf("wireclient: URL is required")
	}
	if opts.Token == "" {
		return nil, fmt.Errorf("wireclient: Token is required")
	}

	dialer := *websocket.DefaultDialer
	var hdr http.Header
	if opts.UseSubprotocol {
		dialer.Subprotocols = []string{"bearer", opts.Token}
	} else {
		hdr = http.Header{}
		hdr.Set("Authorization", "Bearer "+opts.Token)
	}

	conn, resp, err := dialer.Dial(opts.URL, hdr)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("wireclient: dial: %w (status=%d)", err, resp.StatusCode)
		}
		return nil, fmt.Errorf("wireclient: dial: %w", err)
	}
	return &Conn{inner: conn}, nil
}

// ReadEnvelope reads exactly one JSON envelope. A non-zero timeout sets a
// per-frame read deadline; zero clears any deadline previously set by this
// method. The orchestrator only ever sends text frames.
func (c *Conn) ReadEnvelope(timeout time.Duration) (event.Envelope, error) {
	if timeout > 0 {
		_ = c.inner.SetReadDeadline(time.Now().Add(timeout))
	} else {
		_ = c.inner.SetReadDeadline(time.Time{})
	}
	_, data, err := c.inner.ReadMessage()
	if err != nil {
		return event.Envelope{}, err
	}
	return event.DecodeBytes(data)
}

// ReadEnvelopeRaw is ReadEnvelope but also returns the original wire bytes
// alongside the decoded envelope. The CLI smoke tool prints those bytes
// verbatim for human inspection; the mobile app has no reason to use this.
func (c *Conn) ReadEnvelopeRaw(timeout time.Duration) (event.Envelope, []byte, error) {
	if timeout > 0 {
		_ = c.inner.SetReadDeadline(time.Now().Add(timeout))
	} else {
		_ = c.inner.SetReadDeadline(time.Time{})
	}
	_, data, err := c.inner.ReadMessage()
	if err != nil {
		return event.Envelope{}, nil, err
	}
	env, err := event.DecodeBytes(data)
	return env, data, err
}

// WriteEnvelope marshals env to JSON and writes one text frame.
func (c *Conn) WriteEnvelope(env event.Envelope) error {
	b, err := env.Bytes()
	if err != nil {
		return fmt.Errorf("wireclient: marshal envelope: %w", err)
	}
	if err := c.inner.WriteMessage(websocket.TextMessage, b); err != nil {
		return fmt.Errorf("wireclient: write: %w", err)
	}
	return nil
}

// WriteEnvelopeBytes writes a pre-marshaled envelope and returns the bytes
// that went on the wire. Used by the CLI to print the exact frame sent.
func (c *Conn) WriteEnvelopeBytes(env event.Envelope) ([]byte, error) {
	b, err := env.Bytes()
	if err != nil {
		return nil, fmt.Errorf("wireclient: marshal envelope: %w", err)
	}
	if err := c.inner.WriteMessage(websocket.TextMessage, b); err != nil {
		return nil, fmt.Errorf("wireclient: write: %w", err)
	}
	return b, nil
}

// Close closes the underlying WebSocket. Safe to call multiple times.
func (c *Conn) Close() error {
	if c.inner == nil {
		return nil
	}
	return c.inner.Close()
}
