package tracing

import (
	"context"
	"testing"
	"time"
)

func TestInit_Disabled_NoopShutdown(t *testing.T) {
	// Disabled is the production default. Init must return a
	// no-op Shutdown that callers can defer unconditionally,
	// even on the error path.
	shutdown, err := Init(context.Background(), Config{Enabled: false}, nil)
	if err != nil {
		t.Fatalf("Init disabled: %v", err)
	}
	if shutdown == nil {
		t.Fatal("Shutdown must not be nil for disabled config")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("disabled Shutdown returned err: %v", err)
	}
}

func TestInit_EnabledWithBadEndpoint_FallsBackQuietly(t *testing.T) {
	// A typo in the OTLP URL must not take the orchestrator down.
	// otlptracehttp.New is lazy — it doesn't fail at construction
	// time even with a garbage endpoint; the failure only
	// materializes on the first export attempt. Document that
	// behavior by exercising the path.
	shutdown, err := Init(context.Background(), Config{
		Enabled:        true,
		Endpoint:       "definitely-not-a-real-host:9999",
		ServiceName:    "test",
		ServiceVersion: "v0",
		SampleRatio:    1.0,
		Insecure:       true,
		ExportTimeout:  100 * time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatalf("Init with bad endpoint: %v", err)
	}
	if shutdown == nil {
		t.Fatal("Shutdown must not be nil")
	}
	// Shutdown should drain quickly even when nothing was exported.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = shutdown(ctx) // err is acceptable here — the collector was bogus
}

func TestInit_DefaultsFilledIn(t *testing.T) {
	// Empty service name + version + ratio out of range should
	// all get safe defaults. Indirectly verified by Init returning
	// success.
	shutdown, err := Init(context.Background(), Config{
		Enabled:     true,
		SampleRatio: -1.0, // out of range → clamped to 1.0
		Insecure:    true,
		Endpoint:    "localhost:4318",
	}, nil)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if shutdown == nil {
		t.Fatal("Shutdown must not be nil")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = shutdown(ctx)
}
