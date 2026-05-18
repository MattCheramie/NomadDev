package history

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Summarizer condenses a slice of turns into a single string. Implementations
// must be safe for concurrent use.
type Summarizer interface {
	Summarize(ctx context.Context, turns []Turn) (string, error)
}

// SummarizerFunc adapts a plain function to the Summarizer interface — handy
// for tests and for plugging in a closure during wiring.
type SummarizerFunc func(ctx context.Context, turns []Turn) (string, error)

// Summarize calls the underlying function.
func (f SummarizerFunc) Summarize(ctx context.Context, turns []Turn) (string, error) {
	return f(ctx, turns)
}

// HTTPSummarizer POSTs a JSON array of turns to a configured URL and reads a
// `{"summary": "..."}` response. The endpoint is whatever the operator wires
// up via NOMADDEV_HISTORY_SUMMARY_URL.
type HTTPSummarizer struct {
	URL        string
	AuthHeader string // optional, e.g. "Bearer …" — sent as Authorization
	Client     *http.Client
}

// summaryRequestTurn is the wire shape sent to the summarization API. Keeping
// the payload flat (role + text + ts) decouples the API from the storage
// format and avoids leaking the raw parts_json blob shape.
type summaryRequestTurn struct {
	Role string `json:"role"`
	Text string `json:"text"`
	TS   int64  `json:"ts"`
}

type summaryResponse struct {
	Summary string `json:"summary"`
}

// Summarize implements Summarizer. Returns an error when the endpoint is not
// configured, when the HTTP call fails, or when the response is malformed —
// the caller treats any error as "leave the database alone".
func (h *HTTPSummarizer) Summarize(ctx context.Context, turns []Turn) (string, error) {
	if h.URL == "" {
		return "", fmt.Errorf("history: summarizer URL not configured")
	}
	payload := make([]summaryRequestTurn, len(turns))
	for i, t := range turns {
		payload[i] = summaryRequestTurn{
			Role: string(t.Role),
			Text: string(t.Parts),
			TS:   t.TS.UnixNano(),
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("history: marshal summary request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("history: build summary request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if h.AuthHeader != "" {
		req.Header.Set("Authorization", h.AuthHeader)
	}
	client := h.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("history: summary request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("history: summary response %d: %s", resp.StatusCode, string(b))
	}
	var out summaryResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("history: decode summary response: %w", err)
	}
	if strings.TrimSpace(out.Summary) == "" {
		return "", fmt.Errorf("history: empty summary in response")
	}
	return out.Summary, nil
}

// Compactor pairs a SQLiteStore with a Summarizer and word threshold and
// drives periodic compaction. The janitor follows the same shape as
// auth.MemoryRevocationList.RunJanitor and session janitors.
type Compactor struct {
	Store         *SQLiteStore
	Summarizer    Summarizer
	WordThreshold int
}

// RunJanitor ticks every interval until ctx is done. Each tick walks every
// sid in the turns table and asks Compact to do its work. Errors are logged
// and not fatal — the next tick retries naturally.
func (c *Compactor) RunJanitor(ctx context.Context, interval time.Duration, log *slog.Logger) {
	if interval <= 0 || c.Store == nil || c.Summarizer == nil {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.runOnce(ctx, log)
		}
	}
}

// runOnce performs a single sweep across all sessions. Exposed (unexported)
// so tests can drive deterministic compaction without spinning a ticker.
func (c *Compactor) runOnce(ctx context.Context, log *slog.Logger) {
	sids, err := c.Store.distinctSIDs(ctx)
	if err != nil {
		if log != nil {
			log.Warn("history: compactor list sids", "err", err)
		}
		return
	}
	for _, sid := range sids {
		if ctx.Err() != nil {
			return
		}
		n, err := c.Store.Compact(ctx, sid, c.WordThreshold, c.Summarizer)
		if err != nil {
			if log != nil {
				log.Warn("history: compactor", "sid", sid, "err", err)
			}
			continue
		}
		if n > 0 && log != nil {
			log.Info("history: compacted session",
				"sid", sid, "turns_collapsed", n, "threshold_words", c.WordThreshold)
		}
	}
}

// countWords returns a whitespace-split word count, matching the literal
// "15,000 words" budget the operator configures. It's not a token count.
func countWords(s string) int {
	return len(strings.Fields(s))
}

// extractText pulls the "text" field out of a parts_json blob written by
// wsserver/middleware.go's mustMarshalText helper. If the payload isn't in
// that shape — e.g. a future translator stores something richer — fall back
// to the raw bytes so the word count still has signal. Word counts are
// approximate by design.
func extractText(parts []byte) string {
	if len(parts) == 0 {
		return ""
	}
	var envelope struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(parts, &envelope); err == nil && envelope.Text != "" {
		return envelope.Text
	}
	return string(parts)
}

// marshalText produces the {"text": ...} parts_json shape so summary rows
// flow through LoadWindow → translator on the same code path as user and
// assistant turns. Mirrors wsserver/middleware.go mustMarshalText.
func marshalText(text string) []byte {
	b, _ := json.Marshal(struct {
		Text string `json:"text"`
	}{Text: text})
	return b
}
