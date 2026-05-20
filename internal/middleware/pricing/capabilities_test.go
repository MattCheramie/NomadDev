package pricing

import "testing"

func TestSupportsVision_KnownTextOnly(t *testing.T) {
	for _, tc := range []struct {
		provider, model string
	}{
		{"openai", "o3-mini"},
		{"deepseek", "deepseek-chat"},
		{"deepseek", "deepseek-reasoner"},
	} {
		if SupportsVision(tc.provider, tc.model) {
			t.Errorf("%s/%s: want SupportsVision=false (text-only model)", tc.provider, tc.model)
		}
	}
}

func TestSupportsVision_KnownVisionCapable(t *testing.T) {
	// Every other priced model should pass; we don't allowlist explicitly
	// because the source of truth is the textOnlyModels denylist. Just
	// confirm the common default models report as vision-capable.
	for _, tc := range []struct {
		provider, model string
	}{
		{"openai", "gpt-4o-mini"},
		{"openai", "gpt-4o"},
		{"openai", "gpt-4.1"},
		{"anthropic", "claude-sonnet-4-5"},
		{"anthropic", "claude-opus-4-5"},
		{"anthropic", "claude-haiku-4-5"},
		{"gemini", "gemini-2.0-flash"},
		{"deepseek", "deepseek-vl2"},
	} {
		if !SupportsVision(tc.provider, tc.model) {
			t.Errorf("%s/%s: want SupportsVision=true", tc.provider, tc.model)
		}
	}
}

func TestSupportsVision_UnknownIsPermissive(t *testing.T) {
	// We defer to upstream when a model isn't in our table — better than
	// blocking a legitimate new model the operator adopted before we
	// updated this list.
	if !SupportsVision("openai", "gpt-9000-unreleased") {
		t.Error("unknown openai model should default to allowed")
	}
	if !SupportsVision("future-vendor", "any") {
		t.Error("unknown provider should default to allowed")
	}
}

func TestSupportsVision_EmptyDefaults(t *testing.T) {
	// Empty provider/model means the test harness wired Service directly;
	// we allow rather than break the existing unit-test path.
	if !SupportsVision("", "") {
		t.Error("empty pair should default to allowed")
	}
	if !SupportsVision("", "gpt-4o-mini") {
		t.Error("empty provider should default to allowed")
	}
	if !SupportsVision("openai", "") {
		t.Error("empty model should default to allowed")
	}
}
