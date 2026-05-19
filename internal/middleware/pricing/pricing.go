// Package pricing maintains a hard-coded per-(provider, model) USD price
// table for the LLM backends the middleware can speak. It exists so the
// orchestrator can report dollar cost alongside raw token counts on every
// user.intent turn — operators get budget-grade visibility without having
// to maintain external price-mapping infrastructure.
//
// The table reflects public list pricing as of the most recent edit
// (see the comment on the unexported `prices` map). When prices change,
// rebuild the binary. EstimateCostUSD silently returns 0 for any model
// not in the table; the caller (internal/wsserver/middleware.go) logs the
// miss once per (provider, model) so a misspelled NOMADDEV_*_MODEL env
// var doesn't go unnoticed but also doesn't flood the log.
package pricing

import (
	"log/slog"
	"sync"
)

// Price represents the per-million-token list price for one model.
// All values are USD per 1_000_000 tokens.
type Price struct {
	InputUSDPerMillion  float64
	OutputUSDPerMillion float64
}

// prices is the hardcoded price table. Last reviewed against public list
// pricing on 2026-05-19. Sources:
//   - OpenAI:    https://openai.com/api/pricing/
//   - Anthropic: https://www.anthropic.com/pricing#api
//   - DeepSeek:  https://api-docs.deepseek.com/quick_start/pricing
//   - Gemini:    https://ai.google.dev/gemini-api/docs/pricing
//
// Models not listed here are treated as "unknown price" — cost is reported
// as zero and the caller logs once. Adding a new model is intentionally a
// recompile so price changes always show up in a release note.
var prices = map[string]map[string]Price{
	"openai": {
		"gpt-4o-mini":  {InputUSDPerMillion: 0.15, OutputUSDPerMillion: 0.60},
		"gpt-4o":       {InputUSDPerMillion: 2.50, OutputUSDPerMillion: 10.00},
		"gpt-4.1":      {InputUSDPerMillion: 2.00, OutputUSDPerMillion: 8.00},
		"gpt-4.1-mini": {InputUSDPerMillion: 0.40, OutputUSDPerMillion: 1.60},
		"o3-mini":      {InputUSDPerMillion: 1.10, OutputUSDPerMillion: 4.40},
	},
	"anthropic": {
		"claude-sonnet-4-5": {InputUSDPerMillion: 3.00, OutputUSDPerMillion: 15.00},
		"claude-opus-4-5":   {InputUSDPerMillion: 15.00, OutputUSDPerMillion: 75.00},
		"claude-haiku-4-5":  {InputUSDPerMillion: 1.00, OutputUSDPerMillion: 5.00},
	},
	"deepseek": {
		"deepseek-chat":     {InputUSDPerMillion: 0.27, OutputUSDPerMillion: 1.10},
		"deepseek-reasoner": {InputUSDPerMillion: 0.55, OutputUSDPerMillion: 2.19},
	},
	"gemini": {
		"gemini-2.0-flash":      {InputUSDPerMillion: 0.10, OutputUSDPerMillion: 0.40},
		"gemini-2.0-flash-lite": {InputUSDPerMillion: 0.075, OutputUSDPerMillion: 0.30},
	},
}

// EstimateCostUSD returns the dollar cost of one stage's token usage for the
// given (provider, model) tuple. Returns 0 when no entry exists; the caller
// is responsible for warning on a miss (see WarnOnUnknownOnce).
//
// Math is straightforward: prompt * input + completion * output, both
// converted from "USD per 1M tokens" to per-token. No bounds checks because
// translator usage counts are bounded by max_tokens.
func EstimateCostUSD(provider, model string, prompt, completion int64) float64 {
	p, ok := lookup(provider, model)
	if !ok {
		return 0
	}
	const perToken = 1.0 / 1_000_000.0
	return float64(prompt)*p.InputUSDPerMillion*perToken +
		float64(completion)*p.OutputUSDPerMillion*perToken
}

// Lookup returns the Price entry for one (provider, model) tuple, plus an
// ok-bit. Exported for tests + ops-side reporting tools that want to render
// the active table.
func Lookup(provider, model string) (Price, bool) {
	return lookup(provider, model)
}

func lookup(provider, model string) (Price, bool) {
	models, ok := prices[provider]
	if !ok {
		return Price{}, false
	}
	p, ok := models[model]
	return p, ok
}

// KnownModels returns a deep copy of the price table. Used by tests and
// any ops tooling that wants to render the active pricing.
func KnownModels() map[string]map[string]Price {
	out := make(map[string]map[string]Price, len(prices))
	for prov, models := range prices {
		mm := make(map[string]Price, len(models))
		for m, p := range models {
			mm[m] = p
		}
		out[prov] = mm
	}
	return out
}

var (
	unknownWarnOnce sync.Map // key "provider/model" → struct{}
)

// WarnOnUnknownOnce logs a single slog.Warn the first time the orchestrator
// observes a (provider, model) pair that isn't in the price table. Repeated
// calls with the same pair are silent. Pass a nil logger to use slog.Default.
func WarnOnUnknownOnce(logger *slog.Logger, provider, model string) {
	if provider == "" || model == "" {
		return
	}
	if _, ok := lookup(provider, model); ok {
		return
	}
	key := provider + "/" + model
	if _, loaded := unknownWarnOnce.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("middleware/pricing: no entry for (provider, model) — cost will report as 0",
		"provider", provider, "model", model)
}
