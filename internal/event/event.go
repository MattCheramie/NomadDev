// Package event defines the wire envelope and helpers shared by every
// inbound and outbound message on the orchestrator's WebSocket.
package event

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// Monotonic entropy guarantees lexicographic ordering of IDs minted from the
// same process, even within the same millisecond. Protected by idMu because
// MonotonicEntropy is not goroutine-safe.
var (
	idMu      sync.Mutex
	idEntropy = ulid.Monotonic(rand.Reader, 0)
)

// Envelope is the wire format for every message in both directions.
type Envelope struct {
	ID            string          `json:"id"`
	Type          string          `json:"type"`
	TS            time.Time       `json:"ts"`
	CorrelationID string          `json:"correlation_id,omitempty"`
	Payload       json.RawMessage `json:"payload,omitempty"`
}

// ErrMissingType is returned by Decode when an envelope has no `type` field.
var ErrMissingType = errors.New("envelope: missing type")

// NewEnvelope marshals payload to JSON and returns a fresh envelope.
func NewEnvelope(typ string, payload any) (Envelope, error) {
	env := Envelope{
		ID:   NewID(),
		Type: typ,
		TS:   time.Now().UTC(),
	}
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return Envelope{}, fmt.Errorf("event: marshal payload: %w", err)
		}
		env.Payload = b
	}
	return env, nil
}

// NewReply is NewEnvelope but with CorrelationID set to inReplyTo.
func NewReply(typ string, inReplyTo string, payload any) (Envelope, error) {
	env, err := NewEnvelope(typ, payload)
	if err != nil {
		return Envelope{}, err
	}
	env.CorrelationID = inReplyTo
	return env, nil
}

// NewID returns a fresh ULID string. IDs from this function are strictly
// monotonic per process, which the ring buffer relies on for ordering.
func NewID() string {
	idMu.Lock()
	defer idMu.Unlock()
	return ulid.MustNew(ulid.Timestamp(time.Now()), idEntropy).String()
}

// Bytes serializes the envelope to JSON.
func (e Envelope) Bytes() ([]byte, error) {
	return json.Marshal(e)
}

// UnmarshalPayload decodes the envelope's payload into out.
func (e Envelope) UnmarshalPayload(out any) error {
	if len(e.Payload) == 0 {
		return nil
	}
	return json.Unmarshal(e.Payload, out)
}

// Decode reads one JSON envelope from r and validates that `type` is present.
func Decode(r io.Reader) (Envelope, error) {
	var env Envelope
	if err := json.NewDecoder(r).Decode(&env); err != nil {
		return Envelope{}, fmt.Errorf("event: decode: %w", err)
	}
	if env.Type == "" {
		return Envelope{}, ErrMissingType
	}
	return env, nil
}

// DecodeBytes is Decode for an in-memory slice; common in tests and ws read pumps.
func DecodeBytes(b []byte) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return Envelope{}, fmt.Errorf("event: decode: %w", err)
	}
	if env.Type == "" {
		return Envelope{}, ErrMissingType
	}
	return env, nil
}
