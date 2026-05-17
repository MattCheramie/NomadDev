package session

import (
	"github.com/mattcheramie/nomaddev/internal/event"
)

// entry pairs a buffered envelope with its serialized byte cost.
type entry struct {
	env  event.Envelope
	size int
}

// RingBuffer is a bounded FIFO of envelopes capped by both count and total
// bytes. Eviction happens at the head whenever either cap is exceeded.
type RingBuffer struct {
	cap      int
	maxBytes int
	bytes    int
	items    []entry
}

// NewRingBuffer constructs a buffer. A non-positive cap/maxBytes disables
// that limit (size cap of zero would make the buffer useless, so both
// arguments must be positive in practice).
func NewRingBuffer(cap, maxBytes int) *RingBuffer {
	if cap < 1 {
		cap = 1
	}
	if maxBytes < 1 {
		maxBytes = 1
	}
	return &RingBuffer{
		cap:      cap,
		maxBytes: maxBytes,
		items:    make([]entry, 0, cap),
	}
}

// Push appends env to the buffer, evicting oldest entries until both caps hold.
func (b *RingBuffer) Push(env event.Envelope, size int) {
	b.items = append(b.items, entry{env: env, size: size})
	b.bytes += size

	for len(b.items) > b.cap || b.bytes > b.maxBytes {
		if len(b.items) == 1 {
			// One entry over the byte cap — keep it; never go empty after a push.
			return
		}
		b.bytes -= b.items[0].size
		b.items = b.items[1:]
	}
}

// Len returns the current entry count.
func (b *RingBuffer) Len() int { return len(b.items) }

// Bytes returns the current total bytes.
func (b *RingBuffer) Bytes() int { return b.bytes }

// FirstID and LastID return the head/tail envelope ids (or "" if empty).
func (b *RingBuffer) FirstID() string {
	if len(b.items) == 0 {
		return ""
	}
	return b.items[0].env.ID
}

func (b *RingBuffer) LastID() string {
	if len(b.items) == 0 {
		return ""
	}
	return b.items[len(b.items)-1].env.ID
}

// Since returns the envelopes with id strictly greater than lastID, in order.
// stale=true means the buffer has already evicted lastID (or never knew it),
// so the caller must re-initialize state instead of replaying.
//
// Ordering relies on the monotonic ULIDs minted by event.NewID; envelopes
// from a single orchestrator process compare lexicographically in generation
// order.
func (b *RingBuffer) Since(lastID string) (out []event.Envelope, stale bool) {
	if lastID == "" {
		return nil, false
	}
	if len(b.items) == 0 {
		// Client claims to have seen events we have no record of.
		return nil, true
	}
	if lastID < b.items[0].env.ID {
		// Older than the oldest buffered id → evicted.
		return nil, true
	}
	if lastID > b.items[len(b.items)-1].env.ID {
		// Client claims a phantom id newer than anything we minted.
		return nil, true
	}
	for i, it := range b.items {
		if it.env.ID == lastID {
			tail := b.items[i+1:]
			out = make([]event.Envelope, len(tail))
			for j, t := range tail {
				out[j] = t.env
			}
			return out, false
		}
	}
	// In range but no exact match — id never existed in this session.
	return nil, true
}
