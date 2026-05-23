// Package state owns the mobile app's in-memory store and the inbound
// envelope reducer. It mirrors the slices and ingest switch in
// mobile/src/state/store.ts so the native app behaves the same way as the
// React Native SPA from the user's perspective.
//
// Concurrency: the WebSocket session goroutine writes to the Store via
// Update; the UI goroutine reads via Snapshot and reacts to Subscribe
// notifications. All mutations go through a single mutex so neither side
// observes a torn read.
package state

import (
	"sync"

	"github.com/mattcheramie/nomaddev/internal/event"
	"github.com/mattcheramie/nomaddev/internal/wireclient"
)

// TerminalLineCap bounds the lines retained per ToolCall. Older lines roll
// off the front of the slice so a runaway tool call can't grow memory
// unbounded. Mirrors mobile/src/state/store.ts:TOOL_LINE_CAP.
const TerminalLineCap = 2000

// PartialLineCap force-flushes a trailing fragment that never terminated
// with a newline (e.g. a progress bar) so the partial buffer stays bounded.
// Mirrors TOOL_PARTIAL_CAP in the SPA.
const PartialLineCap = 64 * 1024

// MaxImageCount mirrors NOMADDEV_USER_INTENT_MAX_IMAGES in
// internal/wsserver — the orchestrator rejects user.intent envelopes with
// more attachments than this. Enforced client-side so the composer can
// disable the attach button and surface the limit before submit.
const MaxImageCount = 4

// MaxImageBytes mirrors NOMADDEV_USER_INTENT_MAX_IMAGE_BYTES — the
// per-attachment decoded-bytes cap. Decoded bytes are what the orchestrator
// counts; we measure on the raw file we just read off disk, which equals
// the decoded count for the formats we accept (jpeg/png/gif/webp).
const MaxImageBytes = 5 * 1024 * 1024

// TerminalLine is one line of output from a running sandbox tool call,
// classified by source stream. Seq is monotonic per ToolCall — including
// lines that have rolled off the front — and doubles as a stable widget key.
type TerminalLine struct {
	Stream string
	Text   string
	Seq    int
}

// ToolCall tracks one sandbox tool invocation inside a Turn. Lines is the
// completed-line ring (capped at TerminalLineCap); the per-stream Partials
// hold any trailing fragment whose newline hasn't arrived yet. Result is
// nil while the call is in flight.
type ToolCall struct {
	CommandID        string
	Tool             string
	Args             map[string]any
	Lines            []TerminalLine
	LineCount        int
	StdoutPartial    string
	StderrPartial    string
	ElapsedMs        int64
	Result           *event.CommandResultPayload
	AwaitingApproval bool
}

// Turn is one full request/response cycle — user input plus everything the
// assistant produced in reply (streamed prose, tool calls and their output).
// Finished is set when the terminal assistant.message arrives or when an
// error frame closes the turn.
type Turn struct {
	IntentID      string
	UserText      string
	UserImages    []event.ImageInput
	AssistantText string
	ToolCalls     []ToolCall
	Finished      bool
	FinishReason  string
	Error         string
}

// ApprovalPreview is the optional tool-specific dry-run payload that the
// orchestrator copies into a tool.approval.request. apply_code_patch is
// the only tool that populates this today (path / line_number /
// unified_diff, plus optional verify_command from Phase 14).
type ApprovalPreview struct {
	Path          string
	LineNumber    int
	UnifiedDiff   string
	VerifyCommand string
}

// ApprovalRequest is one pending tool.approval.request — the operator needs
// to grant or deny it before the orchestrator dispatches the underlying tool.
// EnvelopeID is the tool.approval.request.id which is used as the
// correlation_id on the tool.approval.{granted,denied} reply.
type ApprovalRequest struct {
	EnvelopeID       string
	PendingCommandID string
	Tool             string
	Args             map[string]any
	Reason           string
	DeadlineUnixMs   int64
	Preview          *ApprovalPreview
}

// SessionTokens mirrors the per-turn LLM usage panel the mobile Settings
// screen renders. Updated cumulatively across the session, never reset.
type SessionTokens struct {
	Prompt     int64
	Candidates int64
	Total      int64
	CostUSD    float64
}

// State is the immutable snapshot exposed by Store.Snapshot.
type State struct {
	ServerURL        string
	Token            string
	SessionID        string
	Status           wireclient.Status
	Turns            []Turn
	PendingApprovals []ApprovalRequest
	// PendingImages are attachments the user has picked but not yet sent.
	// The composer renders thumbnails for these and they ship out with
	// the next user.intent envelope via TakePendingImages.
	PendingImages   []event.ImageInput
	Provider        string
	Model           string
	AvailableModels []string
	SessionTokens   SessionTokens
	LastEventID     string
	LastError       string
}

// Store holds the app's mutable state behind a mutex and notifies
// subscribers on every change. UI code drives layout from Snapshot.
type Store struct {
	mu    sync.RWMutex
	state State
	subs  map[chan struct{}]struct{}
}

// New returns a fresh Store in the idle state.
func New() *Store {
	return &Store{
		state: State{Status: wireclient.StatusIdle},
		subs:  make(map[chan struct{}]struct{}),
	}
}

// Snapshot returns a value copy of the current state. The Turns slice is
// shared by reference; callers must treat it as read-only.
func (s *Store) Snapshot() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// Subscribe returns a channel that receives a non-blocking signal whenever
// the state changes, plus an unsubscribe func. The channel is buffered to 1
// so a slow subscriber only ever misses redundant signals — the next read
// will see the latest state.
func (s *Store) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}
}

// Update runs fn against an addressable State copy and commits it as the
// new state atomically. Subscribers are notified once.
func (s *Store) Update(fn func(*State)) {
	s.mu.Lock()
	fn(&s.state)
	subs := make([]chan struct{}, 0, len(s.subs))
	for ch := range s.subs {
		subs = append(subs, ch)
	}
	s.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// SetCredentials persists the server URL + token the user typed on the
// Onboard screen. Calling this with empty values clears them ("sign out").
func (s *Store) SetCredentials(serverURL, token string) {
	s.Update(func(st *State) {
		st.ServerURL = serverURL
		st.Token = token
		if token == "" {
			st.SessionID = ""
			st.LastEventID = ""
			st.Turns = nil
			st.PendingApprovals = nil
			st.PendingImages = nil
			st.Status = wireclient.StatusIdle
		}
	})
}

// ImageAttachmentError captures the reason AddPendingImage rejected an
// attachment. The composer surfaces it inline so the user knows whether to
// retry, downsize, or remove a previous attachment.
type ImageAttachmentError string

// Error implements the error interface.
func (e ImageAttachmentError) Error() string { return string(e) }

// Sentinel rejection reasons returned by AddPendingImage. The composer
// matches on errors.Is so messages can be localised without leaking
// internal phrasing.
const (
	ErrImageTooLarge       ImageAttachmentError = "image exceeds the per-attachment size cap"
	ErrTooManyImages       ImageAttachmentError = "too many attachments queued"
	ErrUnsupportedMimeType ImageAttachmentError = "unsupported image type"
)

// AddPendingImage appends an attachment to the composer queue. It enforces
// the count + size + MIME caps that mirror the orchestrator's intent
// validation so the user sees the same answer the server would give.
func (s *Store) AddPendingImage(img event.ImageInput, decodedBytes int) error {
	if !isAcceptedImageMIME(img.MediaType) {
		return ErrUnsupportedMimeType
	}
	if decodedBytes > MaxImageBytes {
		return ErrImageTooLarge
	}
	var rejected error
	s.Update(func(st *State) {
		if len(st.PendingImages) >= MaxImageCount {
			rejected = ErrTooManyImages
			return
		}
		st.PendingImages = append(st.PendingImages, img)
	})
	return rejected
}

// RemovePendingImage drops the image at idx, if it exists. Out-of-range
// indices are a silent no-op so a stale tap from a stale frame doesn't
// crash.
func (s *Store) RemovePendingImage(idx int) {
	s.Update(func(st *State) {
		if idx < 0 || idx >= len(st.PendingImages) {
			return
		}
		st.PendingImages = append(st.PendingImages[:idx], st.PendingImages[idx+1:]...)
	})
}

// TakePendingImages returns the queued attachments and atomically clears
// them — the composer calls this on Send so the next turn starts empty.
func (s *Store) TakePendingImages() []event.ImageInput {
	var out []event.ImageInput
	s.Update(func(st *State) {
		out = st.PendingImages
		st.PendingImages = nil
	})
	return out
}

func isAcceptedImageMIME(mt string) bool {
	switch mt {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

// PopApproval removes the first approval whose EnvelopeID matches and
// returns it. Returns the zero value and false if nothing matches. The
// caller uses the returned request to compose a tool.approval.granted or
// tool.approval.denied envelope correlated on EnvelopeID.
func (s *Store) PopApproval(envelopeID string) (ApprovalRequest, bool) {
	var (
		out ApprovalRequest
		ok  bool
	)
	s.Update(func(st *State) {
		for i, a := range st.PendingApprovals {
			if a.EnvelopeID == envelopeID {
				out = a
				st.PendingApprovals = append(st.PendingApprovals[:i], st.PendingApprovals[i+1:]...)
				ok = true
				return
			}
		}
	})
	return out, ok
}

// SetStatus updates the connection status. Called from the wireclient.Session
// status callback.
func (s *Store) SetStatus(st wireclient.Status) {
	s.Update(func(s *State) { s.Status = st })
}

// SetLastError records a human-readable error string surfaced on the Onboard
// screen and the chat composer area. Pass "" to clear.
func (s *Store) SetLastError(msg string) {
	s.Update(func(st *State) { st.LastError = msg })
}

// RecordSentIntent appends a new outbound turn to the history. The mobile
// composer calls this on submit so the user's bubble renders immediately,
// before the orchestrator's first assistant.chunk arrives.
func (s *Store) RecordSentIntent(intentID, text string, images []event.ImageInput) {
	s.Update(func(st *State) {
		st.Turns = append(st.Turns, Turn{
			IntentID:   intentID,
			UserText:   text,
			UserImages: images,
		})
	})
}
