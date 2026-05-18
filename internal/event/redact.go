package event

import (
	"fmt"
	"strings"
)

// redactedSentinel is the placeholder we substitute for values of sensitive
// argument keys. Visually distinct enough that an operator scanning the
// approval card knows redaction kicked in.
const redactedSentinel = "[REDACTED]"

// maxArgStringDisplay caps the length of any single string arg in the wire
// envelope. Larger values are truncated with a trailing marker so the
// approval card stays scannable on a phone. The full value still reaches
// the dispatch path — this is display-only.
const maxArgStringDisplay = 4096

// sensitiveArgKeys is the set of arg-key substrings (case-insensitive) that
// trigger value redaction. We match by substring rather than equality so
// things like "github_token", "client_secret", "private_key_pem" all
// catch. Defensive default — most github_* tools don't pass credentials
// in args (the PAT comes from env) but custom backends might.
var sensitiveArgKeys = []string{
	"password",
	"passwd",
	"secret",
	"token",
	"apikey",
	"api_key",
	"auth",
	"authorization",
	"credential",
	"private_key",
	"client_secret",
	"bearer",
	"pat",
	"x-api-key",
}

// RedactArgs returns a defensive copy of args with values for sensitive keys
// replaced by [REDACTED] and oversized strings truncated. The original
// map is never mutated — callers can safely pass the same args to the
// dispatch layer afterward. Maps are walked recursively so a credential
// nested inside a {config: {token: "..."}} dict is still caught.
//
// nil input → nil output (the caller can wire either through to the wire
// envelope; both render as JSON `null`).
func RedactArgs(args map[string]any) map[string]any {
	if args == nil {
		return nil
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		out[k] = redactValue(k, v)
	}
	return out
}

func redactValue(key string, v any) any {
	if isSensitiveKey(key) {
		// Redact regardless of underlying type — credentials masquerading
		// as numbers or arrays are still credentials.
		return redactedSentinel
	}
	switch x := v.(type) {
	case string:
		return truncateString(x)
	case map[string]any:
		return RedactArgs(x)
	case []any:
		out := make([]any, len(x))
		for i, elem := range x {
			out[i] = redactValue("", elem)
		}
		return out
	}
	return v
}

func isSensitiveKey(key string) bool {
	lower := strings.ToLower(key)
	for _, needle := range sensitiveArgKeys {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// truncateString caps display length without breaking the JSON shape —
// callers JSON-marshal the redacted map and can't tolerate raw line
// breaks. The trailing marker is plain ASCII so it round-trips through
// any JSON encoder.
func truncateString(s string) string {
	if len(s) <= maxArgStringDisplay {
		return s
	}
	return fmt.Sprintf("%s… (%d bytes truncated for display)",
		s[:maxArgStringDisplay], len(s)-maxArgStringDisplay)
}
