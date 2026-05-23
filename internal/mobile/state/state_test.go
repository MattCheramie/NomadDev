package state

import (
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mattcheramie/nomaddev/internal/event"
	"github.com/mattcheramie/nomaddev/internal/wireclient"
)

func TestStore_InitialSnapshot(t *testing.T) {
	st := New().Snapshot()
	if st.Status != wireclient.StatusIdle {
		t.Fatalf("initial status = %q, want idle", st.Status)
	}
	if len(st.Turns) != 0 {
		t.Fatalf("initial turns = %d, want 0", len(st.Turns))
	}
}

func TestStore_SetCredentials_ClearsOnEmptyToken(t *testing.T) {
	s := New()
	s.SetCredentials("ws://x/ws", "tok")
	s.Update(func(st *State) {
		st.SessionID = "sess-1"
		st.LastEventID = "evt-1"
		st.Turns = []Turn{{IntentID: "i"}}
		st.Status = wireclient.StatusOpen
	})
	s.SetCredentials("", "")
	got := s.Snapshot()
	if got.Token != "" || got.SessionID != "" || got.LastEventID != "" || len(got.Turns) != 0 {
		t.Fatalf("sign-out did not clear: %+v", got)
	}
	if got.Status != wireclient.StatusIdle {
		t.Fatalf("sign-out status = %q, want idle", got.Status)
	}
}

func TestStore_Subscribe_NotifiesOnChange(t *testing.T) {
	s := New()
	ch, unsub := s.Subscribe()
	defer unsub()
	s.SetStatus(wireclient.StatusConnecting)
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("subscriber did not receive notification")
	}
}

func TestStore_Subscribe_CoalescesUnreadSignals(t *testing.T) {
	// A slow subscriber should not see one signal per Update — the channel
	// is buffered to 1 and excess signals coalesce. This matters because
	// the UI thread may run at 60 Hz while the WS goroutine fires events
	// faster than that during a streamed assistant turn.
	s := New()
	ch, unsub := s.Subscribe()
	defer unsub()
	for i := 0; i < 20; i++ {
		s.Update(func(st *State) { st.Model = "m" })
	}
	count := 0
loop:
	for {
		select {
		case <-ch:
			count++
		case <-time.After(20 * time.Millisecond):
			break loop
		}
	}
	if count == 0 || count > 2 {
		t.Fatalf("got %d notifications, want 1-2 coalesced", count)
	}
}

func TestIngest_HelloPopulatesSession(t *testing.T) {
	s := New()
	payload, _ := json.Marshal(event.HelloPayload{
		SessionID:       "sess-42",
		Provider:        "gemini",
		Model:           "gemini-1.5-pro",
		AvailableModels: []string{"a", "b"},
	})
	Ingest(s, event.Envelope{ID: "01EVT", Type: event.EventHello, Payload: payload})
	st := s.Snapshot()
	if st.SessionID != "sess-42" {
		t.Fatalf("SessionID = %q, want sess-42", st.SessionID)
	}
	if st.Model != "gemini-1.5-pro" {
		t.Fatalf("Model = %q", st.Model)
	}
	if len(st.AvailableModels) != 2 {
		t.Fatalf("AvailableModels = %v", st.AvailableModels)
	}
	if st.LastEventID != "01EVT" {
		t.Fatalf("LastEventID = %q, want 01EVT", st.LastEventID)
	}
}

func TestIngest_AssistantChunkAppendsToCorrelatedTurn(t *testing.T) {
	s := New()
	s.RecordSentIntent("01INT", "tell me about go", nil)
	chunk := func(text string) {
		p, _ := json.Marshal(event.AssistantChunkPayload{Text: text})
		Ingest(s, event.Envelope{ID: "x", Type: event.EventAssistantChunk, CorrelationID: "01INT", Payload: p})
	}
	chunk("Go is ")
	chunk("a statically typed ")
	chunk("language.")
	got := s.Snapshot()
	if got.Turns[0].AssistantText != "Go is a statically typed language." {
		t.Fatalf("AssistantText = %q", got.Turns[0].AssistantText)
	}
	if got.Turns[0].Finished {
		t.Fatal("turn should not be finished yet")
	}
}

func TestIngest_AssistantMessageFinishesTurnAndAccumulatesUsage(t *testing.T) {
	s := New()
	s.RecordSentIntent("01INT", "hello", nil)
	p, _ := json.Marshal(event.AssistantMessagePayload{
		Text:         "Hello there.",
		FinishReason: "stop",
		Usage:        &event.UsagePayload{PromptTokens: 12, CandidatesTokens: 5, TotalTokens: 17, CostUSD: 0.0001},
	})
	Ingest(s, event.Envelope{ID: "y", Type: event.EventAssistantMessage, CorrelationID: "01INT", Payload: p})
	got := s.Snapshot()
	if !got.Turns[0].Finished {
		t.Fatal("turn should be finished")
	}
	if got.Turns[0].AssistantText != "Hello there." {
		t.Fatalf("AssistantText = %q", got.Turns[0].AssistantText)
	}
	if got.SessionTokens.Total != 17 {
		t.Fatalf("Total tokens = %d, want 17", got.SessionTokens.Total)
	}
}

func TestIngest_ErrorPopulatesLastError(t *testing.T) {
	s := New()
	p, _ := json.Marshal(event.ErrorPayload{Code: "unauthorized", Message: "token expired"})
	Ingest(s, event.Envelope{ID: "z", Type: event.EventError, Payload: p})
	if got := s.Snapshot().LastError; got != "token expired" {
		t.Fatalf("LastError = %q", got)
	}
}

func TestFileTokenStore_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")
	store := NewFileTokenStore(path)
	if _, _, err := store.Load(); err != ErrNoToken {
		t.Fatalf("Load on empty: err = %v, want ErrNoToken", err)
	}
	if err := store.Save("ws://x/ws", "tok"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	url, tok, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if url != "ws://x/ws" || tok != "tok" {
		t.Fatalf("Load returned (%q,%q)", url, tok)
	}
	if err := store.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if _, _, err := store.Load(); err != ErrNoToken {
		t.Fatalf("Load after Clear: err = %v, want ErrNoToken", err)
	}
}

func TestStore_ConcurrentUpdates_NoTorn(t *testing.T) {
	// Race-detector friendly: many goroutines calling Update on the same
	// store. Ensures the mutex actually serialises mutations.
	s := New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Update(func(st *State) { st.Model += "x" })
		}()
	}
	wg.Wait()
	if len(s.Snapshot().Model) != 50 {
		t.Fatalf("Model len = %d, want 50", len(s.Snapshot().Model))
	}
}
