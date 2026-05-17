package middleware

import (
	"context"
	"strings"
	"testing"

	"github.com/mattcheramie/nomaddev/internal/history"
)

func TestFactory_NoneReturnsNil(t *testing.T) {
	svc, err := NewService(context.Background(), FactoryConfig{Runtime: ""})
	if err != nil || svc != nil {
		t.Fatalf("Runtime=\"\": want (nil, nil), got (%v, %v)", svc, err)
	}
	svc, err = NewService(context.Background(), FactoryConfig{Runtime: RuntimeNone})
	if err != nil || svc != nil {
		t.Fatalf("Runtime=none: want (nil, nil), got (%v, %v)", svc, err)
	}
}

func TestFactory_MockReturnsService(t *testing.T) {
	svc, err := NewService(context.Background(), FactoryConfig{
		Runtime: RuntimeMock,
		History: history.NewMemoryStore(),
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if svc == nil || svc.Translator == nil || svc.Approver == nil || svc.Dispatcher == nil || svc.History == nil {
		t.Fatalf("Service partially built: %+v", svc)
	}
}

func TestFactory_MockRequiresHistory(t *testing.T) {
	_, err := NewService(context.Background(), FactoryConfig{Runtime: RuntimeMock})
	if err == nil || !strings.Contains(err.Error(), "history") {
		t.Fatalf("want history error, got %v", err)
	}
}

func TestFactory_UnknownReturnsError(t *testing.T) {
	_, err := NewService(context.Background(), FactoryConfig{Runtime: "qemu"})
	if err == nil || !strings.Contains(err.Error(), "qemu") {
		t.Fatalf("want unknown-runtime error, got %v", err)
	}
}

func TestFactory_GeminiWithoutTagReturnsError(t *testing.T) {
	svc, err := NewService(context.Background(), FactoryConfig{
		Runtime: RuntimeGemini,
		History: history.NewMemoryStore(),
	})
	if err == nil && svc != nil {
		t.Skip("built with -tags gemini; stub test does not apply")
	}
	if err == nil {
		t.Fatal("expected error for gemini runtime without -tags gemini")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "gemini") {
		t.Errorf("error should mention gemini, got %v", err)
	}
}
