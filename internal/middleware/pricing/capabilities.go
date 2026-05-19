package pricing

// SupportsVision reports whether the given (provider, model) is known to
// accept image content blocks on user.intent envelopes. It returns false
// ONLY for models we have explicit evidence don't support vision — e.g.
// OpenAI's o3-mini reasoning model and DeepSeek's text-only chat models.
// Unknown (provider, model) pairs return true: we defer to the upstream
// API to surface any model-specific error rather than block speculatively
// when our local list could be stale.
//
// The orchestrator calls this from internal/wsserver to reject image-bearing
// user.intent envelopes with a clear bad_envelope error when the active
// runtime/model combo can't actually use the images — much friendlier than
// pushing the request through and watching the LLM 4xx with a vague
// "unsupported content block" message.
func SupportsVision(provider, model string) bool {
	if provider == "" || model == "" {
		// Empty pair means the test harness wired Service directly
		// without going through the factory — we can't classify, so
		// fall back to "allow" and let the upstream decide.
		return true
	}
	_, blocked := textOnlyModels[provider+"/"+model]
	return !blocked
}

// textOnlyModels enumerates (provider, model) pairs we know reject image
// inputs. Maintained alongside the price table because both are
// per-(provider, model) lookups operators want to evolve together when a
// new model lands. Keep this list conservative — false positives here
// block legitimate use; false negatives push the diagnostic responsibility
// upstream where it'd land anyway.
var textOnlyModels = map[string]struct{}{
	// OpenAI's o3-mini is a text-only reasoning model. Vision-bearing
	// requests come back as HTTP 400 "Invalid content type 'image_url'
	// for model 'o3-mini'".
	"openai/o3-mini": {},

	// DeepSeek's standard chat + reasoner models are text-only. Vision
	// is served by deepseek-vl2 on a separate model name (added to the
	// price table — see prices map below).
	"deepseek/deepseek-chat":     {},
	"deepseek/deepseek-reasoner": {},
}
