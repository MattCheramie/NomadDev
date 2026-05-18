package event

import (
	"fmt"
	"regexp"
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
		// Script-shaped args carry shell code; an inline
		// `export TOKEN=abc123` would otherwise show in plain text
		// on the approval card. Scan and mask any sensitive-named
		// assignments before truncation. Non-script string keys
		// fall through to the standard truncation path.
		if isScriptKey(key) {
			return truncateString(redactScript(x))
		}
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

// scriptKeys are arg-key substrings (case-insensitive) that signal
// "this string is shell code, scan its contents for inline secrets".
// Kept narrow on purpose — a free-form "description" or "body" field
// shouldn't get pattern-scanned because the scanner is a heuristic,
// not a parser, and false positives mangle prose.
var scriptKeys = []string{"script", "command"}

func isScriptKey(key string) bool {
	if key == "" {
		return false
	}
	lower := strings.ToLower(key)
	for _, needle := range scriptKeys {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// inlineAssignRE matches shell-style `NAME=VALUE` and
// `export NAME=VALUE` (and `set NAME=VALUE`) — the common shapes
// for setting a secret inline in a bash script. The NAME capture is
// strict (uppercase letters, digits, underscore) so we don't match
// prose like "the script=foo argument"; the VALUE capture is greedy
// to end-of-line OR end-of-quoted-string, covering both
// `TOKEN=abc` and `TOKEN="abc 123"`. Heuristic on purpose; the
// approval card stays useful even when this misses a creative
// assignment shape — operators can read the script.
var inlineAssignRE = regexp.MustCompile(
	`(?m)(?:\b(?:export|set)\s+)?([A-Z][A-Z0-9_]*)=(?:("[^"]*"|'[^']*'|[^\s;&|]+))`,
)

// redactScript scans s for inline NAME=VALUE assignments and replaces
// the VALUE with the redaction sentinel when NAME matches the same
// sensitive-key list RedactArgs uses. Returns s unchanged when no
// matches are found.
func redactScript(s string) string {
	return inlineAssignRE.ReplaceAllStringFunc(s, func(match string) string {
		// Re-run the regex to pull out the capture groups; ReplaceAllStringFunc
		// doesn't expose them directly.
		sub := inlineAssignRE.FindStringSubmatch(match)
		if len(sub) < 3 {
			return match
		}
		name := sub[1]
		if !isSensitiveKey(name) {
			return match
		}
		// Reconstruct the prefix exactly so the original whitespace
		// + export/set keyword survives — operators reviewing the
		// approval need the script to remain readable.
		eq := strings.Index(match, "=")
		if eq < 0 {
			return match
		}
		return match[:eq+1] + redactedSentinel
	})
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
