// Package tracing wires OpenTelemetry into the orchestrator. Default is
// a no-op tracer that the rest of the codebase can call into safely;
// when NOMADDEV_OTEL_ENABLED is true, Init returns a TracerProvider
// that ships spans over OTLP/HTTP to NOMADDEV_OTEL_OTLP_ENDPOINT.
//
// Call sites use otel.Tracer("…") at package scope and Start spans
// where useful (one root span per inbound envelope; child spans for
// sandbox.Exec / githubmcp.Call / approval round-trips). The
// instrumentation is intentionally narrow — broad spans on every
// internal hop drown out the signal and add wall-clock overhead.
package tracing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Config is the operator-facing knob set, populated from
// NOMADDEV_OTEL_* env vars in internal/config.
type Config struct {
	// Enabled toggles tracing on/off. Disabled is the default —
	// Init returns a no-op TracerProvider and a no-op Shutdown.
	Enabled bool
	// Endpoint is the OTLP/HTTP collector URL, e.g.
	// "http://otel-collector.observability.svc:4318". When empty,
	// otlptracehttp uses its own default (localhost:4318).
	Endpoint string
	// ServiceName is the resource attribute on every span. Picked
	// up by collectors as the canonical "what's emitting this".
	ServiceName string
	// ServiceVersion is the orchestrator's build tag (the same
	// string main.version carries). Empty leaves it as "dev".
	ServiceVersion string
	// SampleRatio is the head-based sampling ratio (0.0–1.0). 1.0
	// captures every span; production usually wants something
	// closer to 0.1 to keep collector cost bounded.
	SampleRatio float64
	// ExportTimeout caps how long the batch exporter waits before
	// dropping spans during shutdown. 5s is the otel default.
	ExportTimeout time.Duration
	// Insecure, when true, lets the HTTP exporter talk to a
	// plain-HTTP collector. Required when the operator runs the
	// collector on the same tailnet (Tailscale provides the
	// transport encryption — see docs/auth.md TLS section).
	Insecure bool
}

// Shutdown is the cleanup hook returned by Init. Callers defer it;
// nil shutdowns are safe to call.
type Shutdown func(ctx context.Context) error

// Init wires the global TracerProvider. Returns a no-op Shutdown
// when cfg.Enabled is false so callers can `defer shutdown(ctx)`
// unconditionally. Init never panics on misconfiguration — it
// logs a warning and falls back to the no-op tracer so a typo
// in the OTLP URL doesn't take the orchestrator down.
func Init(ctx context.Context, cfg Config, log *slog.Logger) (Shutdown, error) {
	if !cfg.Enabled {
		return func(context.Context) error { return nil }, nil
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = "nomaddev-orchestrator"
	}
	if cfg.ServiceVersion == "" {
		cfg.ServiceVersion = "dev"
	}
	if cfg.SampleRatio < 0 || cfg.SampleRatio > 1 {
		cfg.SampleRatio = 1.0
	}
	if cfg.ExportTimeout <= 0 {
		cfg.ExportTimeout = 5 * time.Second
	}

	// Build the OTLP/HTTP exporter. Insecure + endpoint are the
	// two knobs operators care about; everything else is
	// otlptracehttp defaults.
	opts := []otlptracehttp.Option{}
	if cfg.Endpoint != "" {
		opts = append(opts, otlptracehttp.WithEndpoint(cfg.Endpoint))
	}
	if cfg.Insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	exp, err := otlptrace.New(ctx, otlptracehttp.NewClient(opts...))
	if err != nil {
		if log != nil {
			log.Warn("tracing: exporter init failed; tracing disabled",
				"err", err, "endpoint", cfg.Endpoint)
		}
		return func(context.Context) error { return nil }, nil
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.ServiceVersion),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("tracing: build resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp, sdktrace.WithBatchTimeout(cfg.ExportTimeout)),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(
			sdktrace.TraceIDRatioBased(cfg.SampleRatio),
		)),
	)
	otel.SetTracerProvider(tp)

	if log != nil {
		log.Info("tracing: enabled",
			"endpoint", cfg.Endpoint,
			"service", cfg.ServiceName,
			"sample_ratio", cfg.SampleRatio)
	}

	return func(shutdownCtx context.Context) error {
		// Flush pending spans then close. Errors are joined so the
		// caller sees both failures if both legs trip.
		var errs []error
		if err := tp.Shutdown(shutdownCtx); err != nil {
			errs = append(errs, fmt.Errorf("tracer provider shutdown: %w", err))
		}
		return errors.Join(errs...)
	}, nil
}
