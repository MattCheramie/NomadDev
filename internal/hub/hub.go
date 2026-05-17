// Package hub is the orchestrator's connection registry. A single goroutine
// owns the per-client maps so the rest of the orchestrator never races on them.
package hub

import (
	"context"
	"errors"
	"log/slog"

	"github.com/mattcheramie/nomaddev/internal/event"
)

// ErrUnknownSession is returned by SendToSession when no client is registered
// for the given SID.
var ErrUnknownSession = errors.New("hub: no client for session id")

// Hub multiplexes events across registered clients.
type Hub struct {
	register   chan *Client
	unregister chan *Client
	sendBySID  chan sidEnvelope
	log        *slog.Logger
}

type sidEnvelope struct {
	sid string
	env event.Envelope
	err chan error
}

// New constructs a Hub. Call Run(ctx) in a goroutine before registering clients.
func New(log *slog.Logger) *Hub {
	return &Hub{
		register:   make(chan *Client, 8),
		unregister: make(chan *Client, 8),
		sendBySID:  make(chan sidEnvelope, 32),
		log:        log,
	}
}

// Register schedules c for registration. Safe from any goroutine.
func (h *Hub) Register(c *Client) { h.register <- c }

// Unregister schedules c for removal. Safe from any goroutine.
func (h *Hub) Unregister(c *Client) { h.unregister <- c }

// SendToSession delivers env to the client currently bound to sid.
// Returns ErrUnknownSession if no client matches.
func (h *Hub) SendToSession(sid string, env event.Envelope) error {
	errCh := make(chan error, 1)
	h.sendBySID <- sidEnvelope{sid: sid, env: env, err: errCh}
	return <-errCh
}

// Run is the hub's owner goroutine. It returns when ctx is cancelled, draining
// all registered clients on the way out.
func (h *Hub) Run(ctx context.Context) {
	clients := make(map[string]*Client) // keyed by client.ID
	bySID := make(map[string]*Client)

	for {
		select {
		case <-ctx.Done():
			for _, c := range clients {
				c.Close()
			}
			return

		case c := <-h.register:
			if prev, ok := bySID[c.SID]; ok && prev.ID != c.ID {
				h.log.Info("hub: replacing session", "sid", c.SID, "old", prev.ID, "new", c.ID)
				replaced, err := event.NewEnvelope(event.EventSessionReplaced, nil)
				if err == nil {
					select {
					case prev.Send <- replaced:
					default:
					}
				}
				prev.Close()
				delete(clients, prev.ID)
			}
			clients[c.ID] = c
			bySID[c.SID] = c
			h.log.Info("hub: registered", "client_id", c.ID, "sid", c.SID, "total", len(clients))

		case c := <-h.unregister:
			if _, ok := clients[c.ID]; ok {
				delete(clients, c.ID)
				if current, ok := bySID[c.SID]; ok && current.ID == c.ID {
					delete(bySID, c.SID)
				}
				c.Close()
				h.log.Info("hub: unregistered", "client_id", c.ID, "sid", c.SID, "total", len(clients))
			}

		case msg := <-h.sendBySID:
			c, ok := bySID[msg.sid]
			if !ok {
				msg.err <- ErrUnknownSession
				continue
			}
			select {
			case c.Send <- msg.env:
				msg.err <- nil
			default:
				msg.err <- errors.New("hub: client send buffer full")
			}
		}
	}
}
