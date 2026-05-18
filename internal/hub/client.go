package hub

import (
	"sync"

	"github.com/mattcheramie/nomaddev/internal/event"
)

// Client is the orchestrator's handle on one connected WebSocket. The hub
// owns the registration lifecycle; pumps in internal/wsserver own the bytes.
type Client struct {
	ID     string   // unique per socket
	SID    string   // session id (sticky across reconnects)
	Sub    string   // authenticated user identity (JWT sub claim); empty for unauth contexts
	Scopes []string // JWT scope claim; used by Phase 12 per-tool authz
	Send   chan event.Envelope

	closeOnce sync.Once
	closed    chan struct{}
}

// NewClient returns a Client with a buffered Send channel. sub may be empty
// when the caller doesn't need per-user routing (e.g., test fixtures); the
// real wsserver path always sets it from claims.Sub. Pre-Phase-12 callers
// that don't pass scopes get an empty slice — back-compat with the legacy
// "any tool" model (see auth.HasToolScope).
func NewClient(id, sid, sub string, sendBuf int) *Client {
	return NewClientWithScopes(id, sid, sub, nil, sendBuf)
}

// NewClientWithScopes is NewClient plus the JWT scope claim. The
// wsserver layer wires this so Phase 12's per-tool scope check has
// the claim available at every dispatch site.
func NewClientWithScopes(id, sid, sub string, scopes []string, sendBuf int) *Client {
	return &Client{
		ID:     id,
		SID:    sid,
		Sub:    sub,
		Scopes: scopes,
		Send:   make(chan event.Envelope, sendBuf),
		closed: make(chan struct{}),
	}
}

// Close idempotently signals the write pump to exit. It does NOT close Send;
// callers (e.g. async sandbox handlers) treat Done() as the cancellation
// signal and use a non-blocking send so a slow/dead client never blocks.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		close(c.closed)
	})
}

// Done is closed when Close has been called.
func (c *Client) Done() <-chan struct{} { return c.closed }

// IsClosed reports whether Close has been called. Safe from any goroutine.
func (c *Client) IsClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}
