package sandbox

import (
	"context"
	"strings"
	"testing"
)

func TestFactory_NoneReturnsNil(t *testing.T) {
	r, err := NewRunner(context.Background(), FactoryConfig{Runtime: ""})
	if err != nil {
		t.Fatalf("Runtime=\"\": err=%v", err)
	}
	if r != nil {
		t.Fatalf("Runtime=\"\": want nil Runner, got %T", r)
	}

	r, err = NewRunner(context.Background(), FactoryConfig{Runtime: RuntimeNone})
	if err != nil {
		t.Fatalf("Runtime=none: err=%v", err)
	}
	if r != nil {
		t.Fatalf("Runtime=none: want nil Runner, got %T", r)
	}
}

func TestFactory_MockReturnsRunner(t *testing.T) {
	r, err := NewRunner(context.Background(), FactoryConfig{Runtime: RuntimeMock})
	if err != nil {
		t.Fatalf("Runtime=mock: err=%v", err)
	}
	if r == nil {
		t.Fatalf("Runtime=mock: nil Runner")
	}
	ch, err := r.Exec(context.Background(), ExecRequest{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	for range ch { // ensure it terminates cleanly
	}
}

func TestFactory_UnknownReturnsError(t *testing.T) {
	_, err := NewRunner(context.Background(), FactoryConfig{Runtime: "qemu"})
	if err == nil {
		t.Fatal("expected error for unknown runtime")
	}
	if !strings.Contains(err.Error(), "qemu") {
		t.Errorf("error should mention the unknown runtime, got %v", err)
	}
}

func TestFactory_DockerWithoutTagReturnsError(t *testing.T) {
	// This file compiles only without the `docker` build tag (the stub lives
	// in factory_nodocker.go). With the tag the test runs against the real
	// implementation, which returns a different error (or success) — skip in
	// that case rather than fail.
	r, err := NewRunner(context.Background(), FactoryConfig{Runtime: RuntimeDocker})
	if err == nil && r != nil {
		t.Skip("built with -tags docker; stub-only test does not apply")
	}
	if err == nil {
		t.Fatal("expected error for docker runtime without -tags docker")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "docker") {
		t.Errorf("error should mention docker, got %v", err)
	}
}
