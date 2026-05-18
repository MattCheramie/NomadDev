package event

import (
	"strings"
	"testing"
)

func TestRedactScript_MasksSensitiveAssignments(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		want     string
		wantHit  bool
	}{
		{
			name:    "bare assignment",
			in:      "TOKEN=abc123\necho hi",
			want:    "TOKEN=[REDACTED]\necho hi",
			wantHit: true,
		},
		{
			name:    "export prefix",
			in:      "export GITHUB_TOKEN=ghp_abc\nrun",
			want:    "export GITHUB_TOKEN=[REDACTED]\nrun",
			wantHit: true,
		},
		{
			name:    "quoted value",
			in:      `API_KEY="some secret value"` + "\n",
			want:    "API_KEY=[REDACTED]\n",
			wantHit: true,
		},
		{
			name:    "single-quoted value",
			in:      `PASSWORD='pa ss wd'` + "\n",
			want:    "PASSWORD=[REDACTED]\n",
			wantHit: true,
		},
		{
			name:    "non-sensitive name left alone",
			in:      "DEBUG=true\nPORT=8080",
			want:    "DEBUG=true\nPORT=8080",
			wantHit: false,
		},
		{
			name:    "lowercase name (regex requires uppercase) left alone",
			in:      "token=lowercase",
			want:    "token=lowercase",
			wantHit: false,
		},
		{
			name:    "multiple on one line",
			in:      "SECRET=a; OTHER=b; AUTH=c",
			want:    "SECRET=[REDACTED]; OTHER=b; AUTH=[REDACTED]",
			wantHit: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactScript(tc.in)
			if got != tc.want {
				t.Errorf("redactScript(%q):\n got  %q\n want %q", tc.in, got, tc.want)
			}
			if tc.wantHit && !strings.Contains(got, redactedSentinel) {
				t.Errorf("expected at least one redaction sentinel in %q", got)
			}
		})
	}
}

func TestRedactArgs_AppliesScriptRedactionWhenKeyIsScript(t *testing.T) {
	args := map[string]any{
		"shell":  "bash",
		"script": "export TOKEN=abc\necho hi",
	}
	out := RedactArgs(args)
	if got, _ := out["script"].(string); !strings.Contains(got, "[REDACTED]") {
		t.Errorf("script value not redacted: %q", got)
	}
	if got, _ := out["shell"].(string); got != "bash" {
		t.Errorf("non-sensitive shell key got mangled: %q", got)
	}
}

func TestRedactArgs_OriginalNotMutated(t *testing.T) {
	args := map[string]any{
		"script": "TOKEN=abc",
	}
	_ = RedactArgs(args)
	if args["script"].(string) != "TOKEN=abc" {
		t.Errorf("RedactArgs mutated the input: %v", args)
	}
}

func TestRedactArgs_NonScriptKeyDoesNotApplyScriptScan(t *testing.T) {
	// A "body" or "description" field that mentions an env-var
	// assignment in prose must NOT get the scanner — the result is
	// confusing for the operator reading the approval. Only "script"
	// and "command"-keyed values get script-content scanning.
	args := map[string]any{
		"body": "When you SET TOKEN=abc the build breaks",
	}
	out := RedactArgs(args)
	if got, _ := out["body"].(string); got != "When you SET TOKEN=abc the build breaks" {
		t.Errorf("body got mangled: %q", got)
	}
}
