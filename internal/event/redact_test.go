package event

import (
	"reflect"
	"strings"
	"testing"
)

func TestRedactArgs_NilPassthrough(t *testing.T) {
	if got := RedactArgs(nil); got != nil {
		t.Fatalf("nil in → got %v, want nil", got)
	}
}

func TestRedactArgs_KnownSensitiveKeysRedacted(t *testing.T) {
	in := map[string]any{
		"owner":          "matt",
		"repo":           "nomaddev",
		"token":          "ghp_secret_value",
		"password":       "hunter2",
		"api_key":        "ak_xxx",
		"github_token":   "ghp_other",
		"client_secret":  "shh",
		"private_key":    "-----BEGIN...",
		"Authorization":  "Bearer xxx",
		"x-api-key":      "xxx",
		"some_pat_field": "ghp_yet_another",
	}
	out := RedactArgs(in)

	// Non-sensitive passes through unchanged.
	if out["owner"] != "matt" || out["repo"] != "nomaddev" {
		t.Errorf("non-sensitive keys mutated: %v", out)
	}
	for _, k := range []string{
		"token", "password", "api_key", "github_token",
		"client_secret", "private_key", "Authorization",
		"x-api-key", "some_pat_field",
	} {
		if out[k] != redactedSentinel {
			t.Errorf("key %q not redacted: %v", k, out[k])
		}
	}

	// Input map must NOT be mutated.
	if in["token"] != "ghp_secret_value" {
		t.Errorf("RedactArgs mutated the input map")
	}
}

func TestRedactArgs_CaseInsensitive(t *testing.T) {
	out := RedactArgs(map[string]any{
		"PASSWORD": "x", "Token": "y", "AuTh": "z",
	})
	for _, k := range []string{"PASSWORD", "Token", "AuTh"} {
		if out[k] != redactedSentinel {
			t.Errorf("case-mixed key %q not redacted", k)
		}
	}
}

func TestRedactArgs_NestedMaps(t *testing.T) {
	in := map[string]any{
		"config": map[string]any{
			"endpoint": "https://api.example",
			"token":    "ghp_nested",
		},
	}
	out := RedactArgs(in)
	nested := out["config"].(map[string]any)
	if nested["endpoint"] != "https://api.example" {
		t.Errorf("nested non-sensitive mutated: %v", nested)
	}
	if nested["token"] != redactedSentinel {
		t.Errorf("nested sensitive not redacted: %v", nested)
	}
}

func TestRedactArgs_Arrays(t *testing.T) {
	in := map[string]any{
		"items": []any{
			map[string]any{"name": "a", "token": "x"},
			"plain",
		},
	}
	out := RedactArgs(in)
	arr := out["items"].([]any)
	if len(arr) != 2 {
		t.Fatalf("array length = %d", len(arr))
	}
	if m := arr[0].(map[string]any); m["token"] != redactedSentinel || m["name"] != "a" {
		t.Errorf("array element 0 = %v", m)
	}
	if arr[1] != "plain" {
		t.Errorf("array element 1 = %v", arr[1])
	}
}

func TestRedactArgs_TruncatesLongStrings(t *testing.T) {
	long := strings.Repeat("a", maxArgStringDisplay+500)
	out := RedactArgs(map[string]any{"body": long})
	got, _ := out["body"].(string)
	if len(got) >= len(long) {
		t.Fatalf("not truncated: got %d bytes, in %d bytes", len(got), len(long))
	}
	if !strings.Contains(got, "truncated for display") {
		t.Errorf("truncation marker missing: %q", got[len(got)-50:])
	}
}

func TestRedactArgs_PreservesShortStrings(t *testing.T) {
	out := RedactArgs(map[string]any{"body": "short body"})
	if out["body"] != "short body" {
		t.Errorf("short string mutated: %v", out["body"])
	}
}

func TestRedactArgs_DoesNotShareReferences(t *testing.T) {
	// Defensive copy: mutating the output must not bleed back into the
	// input.
	in := map[string]any{"x": map[string]any{"y": "z"}}
	out := RedactArgs(in)
	outNested := out["x"].(map[string]any)
	outNested["y"] = "TAMPERED"
	inNested := in["x"].(map[string]any)
	if inNested["y"] != "z" {
		t.Errorf("redact result shares reference with input: %v", inNested)
	}
}

func TestRedactArgs_RoundTripPreservesNonStringTypes(t *testing.T) {
	in := map[string]any{
		"count":  42,
		"ratio":  3.14,
		"flag":   true,
		"null":   nil,
		"slice":  []any{"a", "b"},
		"object": map[string]any{"k": "v"},
	}
	out := RedactArgs(in)
	if !reflect.DeepEqual(out["count"], 42) {
		t.Errorf("int round-trip: %v", out["count"])
	}
	if !reflect.DeepEqual(out["ratio"], 3.14) {
		t.Errorf("float round-trip: %v", out["ratio"])
	}
	if out["flag"] != true {
		t.Errorf("bool round-trip: %v", out["flag"])
	}
	if out["null"] != nil {
		t.Errorf("nil round-trip: %v", out["null"])
	}
}
