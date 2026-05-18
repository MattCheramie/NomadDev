package wsserver

import (
	"strings"
	"testing"

	"github.com/mattcheramie/nomaddev/internal/event"
	"github.com/mattcheramie/nomaddev/internal/metrics"
	dto "github.com/prometheus/client_model/go"
)

func TestGitHubOutcomeForCode(t *testing.T) {
	cases := []struct {
		code, want string
	}{
		{"", "ok"},
		{event.SandboxErrTimeout, "timeout"},
		{event.SandboxErrCanceled, "canceled"},
		{event.SandboxErrBadRequest, "bad_request"},
		{event.SandboxErrUnauthorized, "denied"},
		{event.SandboxErrInternal, "error"},
		{event.SandboxErrOOM, "error"},
		{"unknown_code", "error"},
	}
	for _, tc := range cases {
		if got := githubOutcomeForCode(tc.code); got != tc.want {
			t.Errorf("githubOutcomeForCode(%q) = %q, want %q", tc.code, got, tc.want)
		}
	}
}

func TestRecordGitHubCall_IgnoresNonGitHub(t *testing.T) {
	before := counterValue(t, "github_irrelevant", "ok")
	recordGitHubCall("execute_script", "")
	recordGitHubCall("read_file", "")
	if got := counterValue(t, "github_irrelevant", "ok"); got != before {
		t.Errorf("non-github tools should not increment the counter")
	}
}

func TestRecordGitHubCall_IncrementsByOutcome(t *testing.T) {
	tool := "github_create_issue_test_only"
	startOK := counterValue(t, tool, "ok")
	startDenied := counterValue(t, tool, "denied")

	recordGitHubCall(tool, "")
	recordGitHubCall(tool, "")
	recordGitHubCall(tool, event.SandboxErrUnauthorized)

	if got := counterValue(t, tool, "ok"); got != startOK+2 {
		t.Errorf("ok delta = %v, want 2", got-startOK)
	}
	if got := counterValue(t, tool, "denied"); got != startDenied+1 {
		t.Errorf("denied delta = %v, want 1", got-startDenied)
	}
}

// counterValue returns the current value of nomaddev_github_calls_total for
// the given tool+outcome labels. Returns 0 when the series hasn't been
// observed yet — Prometheus auto-initialises only at first Inc().
func counterValue(t *testing.T, tool, outcome string) float64 {
	t.Helper()
	mfs, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("registry.Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "nomaddev_github_calls_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			if labelsMatch(m.GetLabel(), tool, outcome) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func labelsMatch(labels []*dto.LabelPair, tool, outcome string) bool {
	var t, o string
	for _, lp := range labels {
		switch lp.GetName() {
		case "tool":
			t = lp.GetValue()
		case "outcome":
			o = lp.GetValue()
		}
	}
	return strings.EqualFold(t, tool) && strings.EqualFold(o, outcome)
}
