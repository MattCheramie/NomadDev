package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler_ExposesPackageInstruments(t *testing.T) {
	// Mutate two distinct instruments so we can grep the scrape output.
	WSConnectsTotal.WithLabelValues("ok").Inc()
	WSActiveConnections.Set(3)
	SessionEventsTotal.WithLabelValues("command.result").Inc()
	SandboxRunsTotal.WithLabelValues("ok").Inc()
	SandboxRunSeconds.Observe(0.123)
	MiddlewareTurnsTotal.WithLabelValues("ok").Inc()
	MiddlewareTurnSeconds.Observe(1.5)
	LLMTokensTotal.WithLabelValues("prompt", "openai", "gpt-4o-mini").Add(100)
	LLMTokensTotal.WithLabelValues("candidates", "openai", "gpt-4o-mini").Add(50)
	LLMTokensTotal.WithLabelValues("total", "openai", "gpt-4o-mini").Add(150)
	LLMCostUSDTotal.WithLabelValues("openai", "gpt-4o-mini").Add(0.075)

	srv := httptest.NewServer(Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	text := string(body)

	wants := []string{
		`nomaddev_ws_connects_total{result="ok"}`,
		`nomaddev_ws_active_connections`,
		`nomaddev_session_events_total{kind="command.result"}`,
		`nomaddev_sandbox_runs_total{outcome="ok"}`,
		`nomaddev_sandbox_run_seconds_bucket`,
		`nomaddev_middleware_turns_total{outcome="ok"}`,
		`nomaddev_middleware_turn_seconds_bucket`,
		`nomaddev_llm_tokens_total{model="gpt-4o-mini",provider="openai",type="prompt"} 100`,
		`nomaddev_llm_tokens_total{model="gpt-4o-mini",provider="openai",type="candidates"} 50`,
		`nomaddev_llm_tokens_total{model="gpt-4o-mini",provider="openai",type="total"} 150`,
		`nomaddev_llm_cost_usd_total{model="gpt-4o-mini",provider="openai"} 0.075`,
	}
	for _, w := range wants {
		if !strings.Contains(text, w) {
			t.Errorf("scrape missing %q", w)
		}
	}
}
