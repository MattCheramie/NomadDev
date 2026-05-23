package state

import (
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mattcheramie/nomaddev/internal/event"
	"github.com/mattcheramie/nomaddev/internal/wireclient"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

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

func TestIngest_CommandRequestAttachesToolCallToTurn(t *testing.T) {
	s := New()
	s.RecordSentIntent("01INT", "do a thing", nil)
	payload, _ := json.Marshal(event.CommandRequestPayload{
		Tool: "execute_script",
		Args: map[string]any{"shell": "bash", "script": "echo hi"},
	})
	Ingest(s, event.Envelope{
		ID: "01CMD", Type: event.EventCommandRequest,
		CorrelationID: "01INT", Payload: payload,
	})
	turn := s.Snapshot().Turns[0]
	if len(turn.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %d, want 1", len(turn.ToolCalls))
	}
	call := turn.ToolCalls[0]
	if call.CommandID != "01CMD" || call.Tool != "execute_script" {
		t.Fatalf("ToolCall = %+v", call)
	}
}

func TestIngest_CommandChunkBuildsLinesAcrossPartials(t *testing.T) {
	s := New()
	s.RecordSentIntent("01INT", "x", nil)
	req, _ := json.Marshal(event.CommandRequestPayload{Tool: "execute_script"})
	Ingest(s, event.Envelope{ID: "01CMD", Type: event.EventCommandRequest, CorrelationID: "01INT", Payload: req})

	chunk := func(stream, data string) {
		p, _ := json.Marshal(event.CommandChunkPayload{Stream: stream, Data: data})
		Ingest(s, event.Envelope{Type: event.EventCommandChunk, CorrelationID: "01CMD", Payload: p})
	}
	// A line split across two chunks plus an interleaved stderr.
	chunk(event.StreamStdout, "hello, ")
	chunk(event.StreamStderr, "warn1\n")
	chunk(event.StreamStdout, "world\nsecond\n")

	call := s.Snapshot().Turns[0].ToolCalls[0]
	if got := len(call.Lines); got != 3 {
		t.Fatalf("Lines = %d, want 3 (got %v)", got, call.Lines)
	}
	if call.Lines[0].Text != "warn1" || call.Lines[0].Stream != event.StreamStderr {
		t.Fatalf("first line = %+v", call.Lines[0])
	}
	if call.Lines[1].Text != "hello, world" || call.Lines[1].Stream != event.StreamStdout {
		t.Fatalf("merged line = %+v", call.Lines[1])
	}
	if call.Lines[2].Text != "second" {
		t.Fatalf("third line = %+v", call.Lines[2])
	}
	if call.StdoutPartial != "" || call.StderrPartial != "" {
		t.Fatalf("partials should be empty: stdout=%q stderr=%q", call.StdoutPartial, call.StderrPartial)
	}
	if call.LineCount != 3 {
		t.Fatalf("LineCount = %d, want 3", call.LineCount)
	}
}

func TestIngest_CommandChunkPartialRollsOver(t *testing.T) {
	s := New()
	s.RecordSentIntent("01INT", "x", nil)
	req, _ := json.Marshal(event.CommandRequestPayload{Tool: "execute_script"})
	Ingest(s, event.Envelope{ID: "01CMD", Type: event.EventCommandRequest, CorrelationID: "01INT", Payload: req})

	// Chunk without trailing newline — should sit in the partial buffer.
	p, _ := json.Marshal(event.CommandChunkPayload{Stream: event.StreamStdout, Data: "no newline yet"})
	Ingest(s, event.Envelope{Type: event.EventCommandChunk, CorrelationID: "01CMD", Payload: p})
	call := s.Snapshot().Turns[0].ToolCalls[0]
	if call.StdoutPartial != "no newline yet" || len(call.Lines) != 0 {
		t.Fatalf("expected partial to hold the data, got partial=%q lines=%v", call.StdoutPartial, call.Lines)
	}
}

func TestMergeChunkIntoToolCall_LineCapDropsOldest(t *testing.T) {
	// Direct exercise of the pure function — easier than driving Ingest
	// thousands of times.
	c := ToolCall{}
	for i := 0; i < TerminalLineCap+5; i++ {
		c = MergeChunkIntoToolCall(c, event.CommandChunkPayload{
			Stream: event.StreamStdout,
			Data:   "line\n",
		})
	}
	if len(c.Lines) != TerminalLineCap {
		t.Fatalf("Lines len = %d, want %d", len(c.Lines), TerminalLineCap)
	}
	if c.LineCount != TerminalLineCap+5 {
		t.Fatalf("LineCount = %d, want %d", c.LineCount, TerminalLineCap+5)
	}
	if c.Lines[0].Seq != 5 {
		t.Fatalf("oldest retained line seq = %d, want 5 (after dropping 5)", c.Lines[0].Seq)
	}
}

func TestIngest_CommandResultClosesCall(t *testing.T) {
	s := New()
	s.RecordSentIntent("01INT", "x", nil)
	req, _ := json.Marshal(event.CommandRequestPayload{Tool: "execute_script"})
	Ingest(s, event.Envelope{ID: "01CMD", Type: event.EventCommandRequest, CorrelationID: "01INT", Payload: req})
	res, _ := json.Marshal(event.CommandResultPayload{ExitCode: 0, DurationMs: 42})
	Ingest(s, event.Envelope{Type: event.EventCommandResult, CorrelationID: "01CMD", Payload: res})
	call := s.Snapshot().Turns[0].ToolCalls[0]
	if call.Result == nil || call.Result.ExitCode != 0 || call.Result.DurationMs != 42 {
		t.Fatalf("Result = %+v", call.Result)
	}
}

func TestIngest_SandboxHeartbeatUpdatesElapsed(t *testing.T) {
	s := New()
	s.RecordSentIntent("01INT", "x", nil)
	req, _ := json.Marshal(event.CommandRequestPayload{Tool: "execute_script"})
	Ingest(s, event.Envelope{ID: "01CMD", Type: event.EventCommandRequest, CorrelationID: "01INT", Payload: req})
	hb, _ := json.Marshal(event.SandboxHeartbeatPayload{ElapsedMs: 1500})
	Ingest(s, event.Envelope{Type: event.EventSandboxHeartbeat, CorrelationID: "01CMD", Payload: hb})
	if got := s.Snapshot().Turns[0].ToolCalls[0].ElapsedMs; got != 1500 {
		t.Fatalf("ElapsedMs = %d, want 1500", got)
	}
}

func TestIngest_ToolApprovalRequestAndPop(t *testing.T) {
	s := New()
	s.RecordSentIntent("01INT", "x", nil)
	req, _ := json.Marshal(event.CommandRequestPayload{Tool: "apply_code_patch"})
	Ingest(s, event.Envelope{ID: "01CMD", Type: event.EventCommandRequest, CorrelationID: "01INT", Payload: req})

	approvalPayload, _ := json.Marshal(event.ToolApprovalRequestPayload{
		Tool:             "apply_code_patch",
		Args:             map[string]any{"path": "x.txt"},
		Reason:           "destructive",
		PendingCommandID: "01CMD",
		TimeoutMs:        45_000,
		Preview: map[string]any{
			"path":         "x.txt",
			"line_number":  float64(10),
			"unified_diff": "@@ -1 +1 @@\n-old\n+new\n",
		},
	})
	Ingest(s, event.Envelope{ID: "01APP", Type: event.EventToolApprovalRequest, Payload: approvalPayload})

	snap := s.Snapshot()
	if len(snap.PendingApprovals) != 1 {
		t.Fatalf("PendingApprovals = %d, want 1", len(snap.PendingApprovals))
	}
	pa := snap.PendingApprovals[0]
	if pa.EnvelopeID != "01APP" || pa.Tool != "apply_code_patch" || pa.Preview == nil ||
		pa.Preview.Path != "x.txt" || pa.Preview.LineNumber != 10 {
		t.Fatalf("ApprovalRequest = %+v preview=%+v", pa, pa.Preview)
	}
	if !snap.Turns[0].ToolCalls[0].AwaitingApproval {
		t.Fatal("ToolCall.AwaitingApproval should be true")
	}

	got, ok := s.PopApproval("01APP")
	if !ok || got.EnvelopeID != "01APP" {
		t.Fatalf("PopApproval returned (%+v, %v)", got, ok)
	}
	if len(s.Snapshot().PendingApprovals) != 0 {
		t.Fatal("PendingApprovals should be empty after Pop")
	}
	if _, ok := s.PopApproval("01APP"); ok {
		t.Fatal("second Pop should miss")
	}
}

func TestStore_AddPendingImage_EnforcesCaps(t *testing.T) {
	s := New()
	img := event.ImageInput{MediaType: "image/png", Data: "abc"}
	for i := 0; i < MaxImageCount; i++ {
		if err := s.AddPendingImage(img, 1024); err != nil {
			t.Fatalf("add #%d: %v", i, err)
		}
	}
	if got := len(s.Snapshot().PendingImages); got != MaxImageCount {
		t.Fatalf("PendingImages len = %d, want %d", got, MaxImageCount)
	}
	if err := s.AddPendingImage(img, 1024); err != ErrTooManyImages {
		t.Fatalf("over-cap add err = %v, want ErrTooManyImages", err)
	}
	if err := s.AddPendingImage(img, MaxImageBytes+1); err != ErrImageTooLarge {
		t.Fatalf("over-bytes add err = %v, want ErrImageTooLarge", err)
	}
	if err := s.AddPendingImage(event.ImageInput{MediaType: "image/heic"}, 100); err != ErrUnsupportedMimeType {
		t.Fatalf("bad-mime add err = %v, want ErrUnsupportedMimeType", err)
	}
}

func TestStore_RemoveAndTakePendingImages(t *testing.T) {
	s := New()
	for _, tag := range []string{"a", "b", "c"} {
		if err := s.AddPendingImage(event.ImageInput{MediaType: "image/png", Data: tag}, 1); err != nil {
			t.Fatal(err)
		}
	}
	s.RemovePendingImage(1) // drop "b"
	left := s.Snapshot().PendingImages
	if len(left) != 2 || left[0].Data != "a" || left[1].Data != "c" {
		t.Fatalf("after Remove, PendingImages = %+v", left)
	}
	// Out-of-range removes are a no-op.
	s.RemovePendingImage(99)
	s.RemovePendingImage(-1)
	if got := len(s.Snapshot().PendingImages); got != 2 {
		t.Fatalf("PendingImages len = %d after no-op removes, want 2", got)
	}
	taken := s.TakePendingImages()
	if len(taken) != 2 {
		t.Fatalf("Take returned %d, want 2", len(taken))
	}
	if got := len(s.Snapshot().PendingImages); got != 0 {
		t.Fatalf("after Take PendingImages = %d, want 0", got)
	}
}

func TestDecodeImageAttachment_PNG(t *testing.T) {
	// Minimal valid PNG header (8 bytes) is enough for http.DetectContentType
	// to identify it; the rest of the data doesn't matter for the wire ship.
	pngHeader := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 1, 2, 3}
	img, n, err := DecodeImageAttachment(bytesReader(pngHeader), "photo.png")
	if err != nil {
		t.Fatalf("DecodeImageAttachment: %v", err)
	}
	if img.MediaType != "image/png" {
		t.Fatalf("MediaType = %q, want image/png", img.MediaType)
	}
	if n != len(pngHeader) {
		t.Fatalf("decoded bytes = %d, want %d", n, len(pngHeader))
	}
	if img.Data == "" {
		t.Fatal("Data should not be empty")
	}
}

func TestDecodeImageAttachment_ExtensionFallback(t *testing.T) {
	// Non-image bytes — DetectContentType returns octet-stream — but the
	// filename hint claims webp, so the helper should believe the
	// extension and pass the wire validator.
	img, _, err := DecodeImageAttachment(bytesReader([]byte("not-actually-webp")), "x.webp")
	if err != nil {
		t.Fatalf("DecodeImageAttachment: %v", err)
	}
	if img.MediaType != "image/webp" {
		t.Fatalf("MediaType = %q, want image/webp via extension", img.MediaType)
	}
}

func TestDecodeImageAttachment_UnsupportedRejected(t *testing.T) {
	_, _, err := DecodeImageAttachment(bytesReader([]byte("plain text")), "notes.txt")
	if err != ErrUnsupportedMimeType {
		t.Fatalf("err = %v, want ErrUnsupportedMimeType", err)
	}
}

func TestStore_ClearPendingImagesOnSignOut(t *testing.T) {
	s := New()
	if err := s.AddPendingImage(event.ImageInput{MediaType: "image/png", Data: "x"}, 1); err != nil {
		t.Fatal(err)
	}
	s.SetCredentials("", "")
	if got := len(s.Snapshot().PendingImages); got != 0 {
		t.Fatalf("after sign-out PendingImages = %d, want 0", got)
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
