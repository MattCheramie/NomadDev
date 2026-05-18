package history

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// helper: build a {"text":"<words>"} parts blob of n whitespace-separated words.
func wordsBlob(prefix string, n int) []byte {
	parts := make([]string, n)
	for i := 0; i < n; i++ {
		parts[i] = fmt.Sprintf("%s%d", prefix, i)
	}
	return marshalText(strings.Join(parts, " "))
}

func TestCompact_BelowThreshold_NoOp(t *testing.T) {
	s := newSQLiteStore(t)
	ctx := context.Background()

	if _, err := s.Append(ctx, Turn{SID: "sid", Role: RoleUser, Parts: wordsBlob("u", 100)}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := s.Append(ctx, Turn{SID: "sid", Role: RoleAssistant, Parts: wordsBlob("a", 100)}); err != nil {
		t.Fatalf("append: %v", err)
	}

	var called atomic.Int32
	summer := SummarizerFunc(func(ctx context.Context, turns []Turn) (string, error) {
		called.Add(1)
		return "should not be called", nil
	})

	n, err := s.Compact(ctx, "sid", 1000, summer)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if n != 0 {
		t.Fatalf("want 0 collapsed, got %d", n)
	}
	if called.Load() != 0 {
		t.Fatalf("summarizer should not have been called")
	}
	rows, _ := s.LoadWindow(ctx, "sid", 0)
	if len(rows) != 2 {
		t.Fatalf("rows changed: %d", len(rows))
	}
}

func TestCompact_AboveThreshold_ReplacesOldestHalf(t *testing.T) {
	s := newSQLiteStore(t)
	ctx := context.Background()

	// 6 user/assistant turns at 50 words each = 300 words total.
	// Threshold 100 → trigger; oldest 3 (idx 0,1,2) get collapsed.
	for i := 0; i < 6; i++ {
		role := RoleUser
		if i%2 == 1 {
			role = RoleAssistant
		}
		if _, err := s.Append(ctx, Turn{
			SID: "sid", Role: role, Parts: wordsBlob("w", 50),
			TS: time.Unix(0, int64(i+1)).UTC(),
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	// Add a tool_call/tool_result pair interleaved at the start — they must
	// survive untouched (not eligible for summarization).
	if _, err := s.Append(ctx, Turn{SID: "sid", Role: RoleToolCall, Parts: marshalText("tc")}); err != nil {
		t.Fatalf("append tc: %v", err)
	}
	if _, err := s.Append(ctx, Turn{SID: "sid", Role: RoleToolResult, Parts: marshalText("tr")}); err != nil {
		t.Fatalf("append tr: %v", err)
	}

	var capturedRoles []Role
	summer := SummarizerFunc(func(ctx context.Context, turns []Turn) (string, error) {
		for _, t := range turns {
			capturedRoles = append(capturedRoles, t.Role)
		}
		return "SUMMARY", nil
	})

	n, err := s.Compact(ctx, "sid", 100, summer)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if n != 3 {
		t.Fatalf("want 3 collapsed, got %d", n)
	}
	for _, r := range capturedRoles {
		if r != RoleUser && r != RoleAssistant {
			t.Fatalf("summarizer received non user/assistant role: %s", r)
		}
	}

	rows, err := s.LoadWindow(ctx, "sid", 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// Expect: 1 system.summary at idx 0, the 2 tool rows (idx 6,7),
	// and the 3 surviving user/assistant rows (idx 3,4,5) — 6 total.
	if len(rows) != 6 {
		t.Fatalf("want 6 rows post-compaction, got %d: %+v", len(rows), rows)
	}
	if rows[0].Role != RoleSystemSummary || rows[0].Idx != 0 {
		t.Fatalf("summary not at idx 0: %+v", rows[0])
	}
	var summaryEnvelope struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(rows[0].Parts, &summaryEnvelope); err != nil {
		t.Fatalf("summary parts not JSON: %v", err)
	}
	if summaryEnvelope.Text != "SUMMARY" {
		t.Fatalf("summary text = %q, want SUMMARY", summaryEnvelope.Text)
	}

	// tool rows preserved.
	var sawToolCall, sawToolResult bool
	for _, r := range rows {
		if r.Role == RoleToolCall {
			sawToolCall = true
		}
		if r.Role == RoleToolResult {
			sawToolResult = true
		}
	}
	if !sawToolCall || !sawToolResult {
		t.Fatalf("tool turns lost: %+v", rows)
	}

	// Subsequent Append must continue past the highest existing idx, not
	// reuse the gap left by the deleted rows.
	idx, err := s.Append(ctx, Turn{SID: "sid", Role: RoleUser, Parts: marshalText("next")})
	if err != nil {
		t.Fatalf("append after compaction: %v", err)
	}
	if idx != 8 {
		t.Fatalf("next idx = %d, want 8 (MAX+1 from idx 7)", idx)
	}
}

func TestCompact_SummarizerError_LeavesDBUntouched(t *testing.T) {
	s := newSQLiteStore(t)
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		role := RoleUser
		if i%2 == 1 {
			role = RoleAssistant
		}
		if _, err := s.Append(ctx, Turn{SID: "sid", Role: role, Parts: wordsBlob("w", 50)}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	summer := SummarizerFunc(func(ctx context.Context, turns []Turn) (string, error) {
		return "", fmt.Errorf("upstream down")
	})

	if _, err := s.Compact(ctx, "sid", 100, summer); err == nil {
		t.Fatalf("expected error from summarizer to surface")
	}
	rows, _ := s.LoadWindow(ctx, "sid", 0)
	if len(rows) != 4 {
		t.Fatalf("rows changed despite summarizer error: %d", len(rows))
	}
	for _, r := range rows {
		if r.Role == RoleSystemSummary {
			t.Fatalf("summary inserted on error path: %+v", r)
		}
	}
}

func TestCompact_ConcurrentAppendsSerialized(t *testing.T) {
	s := newSQLiteStore(t)
	ctx := context.Background()

	// Pre-populate enough words to trigger compaction.
	for i := 0; i < 10; i++ {
		role := RoleUser
		if i%2 == 1 {
			role = RoleAssistant
		}
		if _, err := s.Append(ctx, Turn{SID: "sid", Role: role, Parts: wordsBlob("w", 50)}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	summer := SummarizerFunc(func(ctx context.Context, turns []Turn) (string, error) {
		// Sleep inside the summarizer to widen the race window with the
		// concurrent Append goroutines — the per-SID mutex must hold them
		// off until compaction's transaction commits.
		time.Sleep(50 * time.Millisecond)
		return "SUMMARY", nil
	})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := s.Compact(ctx, "sid", 100, summer); err != nil {
			t.Errorf("compact: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if _, err := s.Append(ctx, Turn{SID: "sid", Role: RoleUser, Parts: marshalText("late")}); err != nil {
				t.Errorf("append: %v", err)
			}
		}
	}()
	wg.Wait()

	// All appends + the summary row must have unique idxs (no PK collision).
	rows, _ := s.LoadWindow(ctx, "sid", 0)
	seen := map[int]bool{}
	for _, r := range rows {
		if seen[r.Idx] {
			t.Fatalf("duplicate idx %d", r.Idx)
		}
		seen[r.Idx] = true
	}
	// Exactly one summary row should exist.
	summaries := 0
	for _, r := range rows {
		if r.Role == RoleSystemSummary {
			summaries++
		}
	}
	if summaries != 1 {
		t.Fatalf("want exactly 1 summary, got %d", summaries)
	}
}

func TestCompact_SurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/history.db"

	{
		s, err := NewSQLiteStore(path)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		for i := 0; i < 4; i++ {
			role := RoleUser
			if i%2 == 1 {
				role = RoleAssistant
			}
			if _, err := s.Append(context.Background(), Turn{SID: "sid", Role: role, Parts: wordsBlob("w", 50)}); err != nil {
				t.Fatalf("append: %v", err)
			}
		}
		summer := SummarizerFunc(func(ctx context.Context, turns []Turn) (string, error) {
			return "SUM", nil
		})
		if _, err := s.Compact(context.Background(), "sid", 100, summer); err != nil {
			t.Fatalf("compact: %v", err)
		}
		_ = s.Close()
	}

	s, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	rows, err := s.LoadWindow(context.Background(), "sid", 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var sawSummary bool
	for _, r := range rows {
		if r.Role == RoleSystemSummary {
			sawSummary = true
		}
	}
	if !sawSummary {
		t.Fatalf("summary row did not survive reopen: %+v", rows)
	}
}

func TestHTTPSummarizer_PostsTurnsAndReturnsSummary(t *testing.T) {
	var gotBody []summaryRequestTurn
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(summaryResponse{Summary: "the gist"})
	}))
	defer srv.Close()

	h := &HTTPSummarizer{URL: srv.URL, AuthHeader: "Bearer xyz"}
	out, err := h.Summarize(context.Background(), []Turn{
		{Role: RoleUser, Parts: []byte("hi there"), TS: time.Unix(0, 1).UTC()},
		{Role: RoleAssistant, Parts: []byte("hello back"), TS: time.Unix(0, 2).UTC()},
	})
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if out != "the gist" {
		t.Fatalf("summary = %q", out)
	}
	if gotAuth != "Bearer xyz" {
		t.Fatalf("auth header = %q", gotAuth)
	}
	if len(gotBody) != 2 || gotBody[0].Role != "user" || gotBody[1].Role != "assistant" {
		t.Fatalf("payload roles wrong: %+v", gotBody)
	}
}

func TestHTTPSummarizer_NonSuccessIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	h := &HTTPSummarizer{URL: srv.URL}
	if _, err := h.Summarize(context.Background(), []Turn{{Role: RoleUser, Parts: []byte("x")}}); err == nil {
		t.Fatalf("expected error on 500")
	}
}

func TestCompactor_RunOnce_VisitsAllSessions(t *testing.T) {
	s := newSQLiteStore(t)
	ctx := context.Background()

	for _, sid := range []string{"alpha", "beta"} {
		for i := 0; i < 4; i++ {
			role := RoleUser
			if i%2 == 1 {
				role = RoleAssistant
			}
			if _, err := s.Append(ctx, Turn{SID: sid, Role: role, Parts: wordsBlob("w", 50)}); err != nil {
				t.Fatalf("append: %v", err)
			}
		}
	}

	var calls atomic.Int32
	c := &Compactor{
		Store:         s,
		Summarizer:    SummarizerFunc(func(ctx context.Context, turns []Turn) (string, error) { calls.Add(1); return "S", nil }),
		WordThreshold: 100,
	}
	c.runOnce(ctx, nil)

	if calls.Load() != 2 {
		t.Fatalf("want 2 summarizer calls (one per sid), got %d", calls.Load())
	}
	for _, sid := range []string{"alpha", "beta"} {
		rows, _ := s.LoadWindow(ctx, sid, 0)
		seen := false
		for _, r := range rows {
			if r.Role == RoleSystemSummary {
				seen = true
			}
		}
		if !seen {
			t.Fatalf("%s missing summary", sid)
		}
	}
}
