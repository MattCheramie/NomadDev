package middleware

import "sort"

// KnownModels returns the static per-provider catalogue the orchestrator
// advertises to clients in HelloPayload.AvailableModels and validates against
// in user.command{set_model}. Adding a new model is a code change here —
// intentional, so the surface stays small and we don't accidentally accept
// a typo'd model name that the upstream SDK would later reject mid-turn.
//
// Keys match the FactoryConfig.Runtime / RuntimeConfig.Provider identifiers.
// The slices are alphabetically sorted for stable wire output.
func KnownModels() map[string][]string {
	return map[string][]string{
		RuntimeOpenAI: {
			"gpt-4o",
			"gpt-4o-mini",
			"o3-mini",
		},
		RuntimeAnthropic: {
			"claude-3-5-haiku-latest",
			"claude-3-5-sonnet-latest",
			"claude-opus-4-5",
			"claude-sonnet-4-5",
		},
		RuntimeGemini: {
			"gemini-1.5-flash",
			"gemini-1.5-pro",
			"gemini-2.0-flash",
		},
		RuntimeDeepSeek: {
			"deepseek-chat",
			"deepseek-coder",
		},
	}
}

// ModelsForProvider returns the catalogue for one provider. Returns nil when
// the provider has no entry (e.g. the mock runtime); callers should treat
// that as "model switching unsupported" rather than an error. The returned
// slice is a fresh copy so callers can sort / append without affecting the
// shared map.
func ModelsForProvider(provider string) []string {
	models, ok := KnownModels()[provider]
	if !ok {
		return nil
	}
	out := make([]string, len(models))
	copy(out, models)
	sort.Strings(out)
	return out
}

// IsKnownModel reports whether model belongs to provider's catalogue. Used by
// the user.command{set_model} handler to reject typos before they reach the
// translator and fail mid-turn.
func IsKnownModel(provider, model string) bool {
	if provider == "" || model == "" {
		return false
	}
	for _, m := range KnownModels()[provider] {
		if m == model {
			return true
		}
	}
	return false
}
