package pricing

import (
	"log/slog"
	"math"
	"testing"
)

func TestEstimateCostUSD_KnownModel(t *testing.T) {
	// One million prompt tokens + one million completion tokens at OpenAI
	// gpt-4o-mini rates: 0.15 + 0.60 = 0.75 USD.
	got := EstimateCostUSD("openai", "gpt-4o-mini", 1_000_000, 1_000_000)
	want := 0.75
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("EstimateCostUSD = %.9f, want %.9f", got, want)
	}
}

func TestEstimateCostUSD_PartialTokens(t *testing.T) {
	// 1500 prompt + 500 completion at Anthropic claude-sonnet-4-5
	// (3.00 / 15.00 USD per 1M):
	//   1500 * 3.00e-6 + 500 * 15.00e-6
	// = 0.0045 + 0.0075 = 0.012
	got := EstimateCostUSD("anthropic", "claude-sonnet-4-5", 1500, 500)
	want := 0.012
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("EstimateCostUSD = %.9f, want %.9f", got, want)
	}
}

func TestEstimateCostUSD_UnknownProvider(t *testing.T) {
	got := EstimateCostUSD("hypothetical", "any", 1_000_000, 1_000_000)
	if got != 0 {
		t.Errorf("EstimateCostUSD unknown provider = %f, want 0", got)
	}
}

func TestEstimateCostUSD_UnknownModel(t *testing.T) {
	got := EstimateCostUSD("openai", "gpt-9000-quantum", 1_000_000, 1_000_000)
	if got != 0 {
		t.Errorf("EstimateCostUSD unknown model = %f, want 0", got)
	}
}

func TestEstimateCostUSD_ZeroTokens(t *testing.T) {
	got := EstimateCostUSD("openai", "gpt-4o-mini", 0, 0)
	if got != 0 {
		t.Errorf("EstimateCostUSD zero tokens = %f, want 0", got)
	}
}

func TestLookup_KnownAndUnknown(t *testing.T) {
	p, ok := Lookup("deepseek", "deepseek-chat")
	if !ok {
		t.Fatal("Lookup deepseek/deepseek-chat: want ok, got false")
	}
	if p.InputUSDPerMillion != 0.27 || p.OutputUSDPerMillion != 1.10 {
		t.Errorf("Lookup price = %+v, want {0.27, 1.10}", p)
	}
	if _, ok := Lookup("deepseek", "deepseek-mythical"); ok {
		t.Error("Lookup deepseek/deepseek-mythical: want false, got true")
	}
}

func TestKnownModels_CoversAllRuntimes(t *testing.T) {
	table := KnownModels()
	for _, provider := range []string{"openai", "anthropic", "deepseek", "gemini"} {
		if len(table[provider]) == 0 {
			t.Errorf("KnownModels: provider %q missing or empty", provider)
		}
	}
	// Confirm the returned map is a copy: mutating it must not affect future calls.
	delete(table, "openai")
	if reread := KnownModels(); len(reread["openai"]) == 0 {
		t.Error("KnownModels returned the live map (mutation leaked)")
	}
}

func TestWarnOnUnknownOnce(t *testing.T) {
	// Smoke test: should not panic with a nil logger, should be safe to
	// call repeatedly. We don't capture log output here — that's covered
	// by the slog default handler in production.
	WarnOnUnknownOnce(nil, "openai", "gpt-9000-quantum")
	WarnOnUnknownOnce(slog.Default(), "openai", "gpt-9000-quantum")
	WarnOnUnknownOnce(nil, "", "anything") // empty provider — silent
	WarnOnUnknownOnce(nil, "openai", "")   // empty model — silent
	// Known model — should be silent regardless of how many calls.
	WarnOnUnknownOnce(nil, "openai", "gpt-4o-mini")
}
