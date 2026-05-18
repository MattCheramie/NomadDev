// Package metrics owns the orchestrator's Prometheus registry and the
// instruments callers reach for from the wsserver / sandbox / middleware
// layers.
//
// All metrics are registered against a package-level *prometheus.Registry so
// the /metrics handler exports exactly the orchestrator's instruments (no
// global default-registry leakage from imports). Build-tag-gated code paths
// can call into this package safely because none of the instruments themselves
// are tag-guarded.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry is the single source of truth for the orchestrator's metrics.
// Tests that need an isolated registry construct their own via prometheus.NewRegistry().
var Registry = prometheus.NewRegistry()

// Connection-layer metrics. result ∈ {"ok", "unauthorized", "upgrade_failed"}.
var (
	WSConnectsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "nomaddev_ws_connects_total",
		Help: "Count of WebSocket upgrade attempts, labeled by outcome.",
	}, []string{"result"})

	WSActiveConnections = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "nomaddev_ws_active_connections",
		Help: "Number of currently-connected WebSocket clients.",
	})
)

// Per-envelope metric. kind is the envelope.Type string.
var SessionEventsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "nomaddev_session_events_total",
	Help: "Count of envelopes appended to per-session replay buffers, labeled by type.",
}, []string{"kind"})

// Sandbox metrics. outcome ∈ {"ok", "timeout", "canceled", "oom",
// "bad_request", "internal", "image_pull"}.
var (
	SandboxRunsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "nomaddev_sandbox_runs_total",
		Help: "Count of sandbox runs that produced a terminal command.result, labeled by outcome.",
	}, []string{"outcome"})

	SandboxRunSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "nomaddev_sandbox_run_seconds",
		Help:    "Sandbox run duration in seconds, end-to-end from runner.Exec entry to terminal chunk.",
		Buckets: prometheus.ExponentialBuckets(0.01, 2, 12), // 10ms → ~40s
	})
)

// Middleware (NLP turn) metrics. outcome ∈ {"ok", "error"}.
var (
	MiddlewareTurnsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "nomaddev_middleware_turns_total",
		Help: "Count of user.intent turns that produced an assistant.message, labeled by outcome.",
	}, []string{"outcome"})

	MiddlewareTurnSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "nomaddev_middleware_turn_seconds",
		Help:    "Wall-clock duration of one user.intent turn, including approval round-trips and tool execs.",
		Buckets: prometheus.ExponentialBuckets(0.05, 2, 12), // 50ms → ~3m
	})
)

// GitHub MCP backend metrics. outcome ∈ {"ok", "error", "denied", "timeout",
// "canceled", "bad_request"} — same outcome strings the sandbox uses, plus
// "denied" for the human-approval-denied path. tool is the prefixed name
// (e.g. "github_create_issue") so dashboards can break down by operation.
//
// High-cardinality warning: ~75 tools × 6 outcomes = ~450 series at most,
// well under Prom's recommended ceiling. If a future GHES build adds many
// custom tools, narrow via NOMADDEV_GITHUB_TOOLSETS rather than dropping the
// label.
var GitHubCallsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "nomaddev_github_calls_total",
	Help: "Count of GitHub MCP tool invocations, labeled by tool name and outcome.",
}, []string{"tool", "outcome"})

func init() {
	Registry.MustRegister(
		WSConnectsTotal,
		WSActiveConnections,
		SessionEventsTotal,
		SandboxRunsTotal,
		SandboxRunSeconds,
		MiddlewareTurnsTotal,
		MiddlewareTurnSeconds,
		GitHubCallsTotal,
	)
}

// Handler returns the /metrics http.Handler bound to the package registry.
// Using HandlerFor with our own registry avoids exporting the default global
// metrics that might leak in from third-party imports.
func Handler() http.Handler {
	return promhttp.HandlerFor(Registry, promhttp.HandlerOpts{Registry: Registry})
}
