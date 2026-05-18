package wsserver

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/mattcheramie/nomaddev/internal/auth"
	"github.com/mattcheramie/nomaddev/internal/config"
	"github.com/mattcheramie/nomaddev/internal/event"
	"github.com/mattcheramie/nomaddev/internal/hub"
	"github.com/mattcheramie/nomaddev/internal/session"
)

// Phase 11.4: a `traceparent` header on the /ws upgrade must lift
// into the per-envelope dispatch span as the parent. Uses an
// in-memory exporter so the test asserts on span structure without
// standing up a real OTLP collector.
func TestTrace_DispatchSpanInheritsTraceparent(t *testing.T) {
	// Install an in-memory exporter + the W3C propagator. Restore
	// the previous globals on teardown so we don't pollute sibling
	// tests in the same package.
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exp),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})

	cfg := &config.Config{
		ListenAddr:   "127.0.0.1:0",
		JWTSecret:    []byte(testSecret),
		LogLevel:     slog.LevelInfo,
		Session:      config.SessionConfig{BufferSize: 32, MaxBytes: 1 << 20},
		Sandbox:      config.SandboxConfig{DefaultTimeout: 2 * time.Second, MaxConcurrent: 4},
		Approval:     config.ApprovalConfig{Timeout: 2 * time.Second},
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 2 * time.Second,
		PingInterval: 30 * time.Second,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := hub.New(logger)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go h.Run(ctx)
	sessions := session.NewMemoryStore(cfg.Session.BufferSize, cfg.Session.MaxBytes)
	verifier := auth.NewVerifier(cfg.JWTSecret)
	srv := New(cfg, logger, h, sessions, verifier, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	issuer := auth.NewIssuer(cfg.JWTSecret, time.Hour)
	tok, _ := issuer.Sign("matt", "sess-1", nil)

	// Mint a synthetic remote parent + inject its traceparent.
	parentTID, _ := trace.TraceIDFromHex("0af7651916cd43dd8448eb211c80319c")
	parentSID, _ := trace.SpanIDFromHex("b7ad6b7169203331")
	parentSC := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    parentTID,
		SpanID:     parentSID,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	carrier := propagation.HeaderCarrier(http.Header{})
	otel.GetTextMapPropagator().Inject(
		trace.ContextWithSpanContext(context.Background(), parentSC),
		carrier,
	)
	if carrier.Get("traceparent") == "" {
		t.Fatal("propagator should have injected traceparent")
	}

	hdrs := http.Header{}
	hdrs.Set("Authorization", "Bearer "+tok)
	hdrs.Set("traceparent", carrier.Get("traceparent"))

	u, _ := url.Parse(ts.URL)
	u.Scheme = "ws"
	u.Path = "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), hdrs)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = readEnv(t, conn) // hello

	// Drive one ping → pong so dispatch starts + ends a span.
	ping, _ := event.NewEnvelope(event.EventPing, event.PingPayload{Nonce: "n1"})
	writeEnv(t, conn, ping)
	_ = readEnv(t, conn)

	// dispatch ends its span synchronously on return; the
	// in-memory exporter is synchronous. Tiny sleep for the read
	// goroutine to finish the dispatch call.
	time.Sleep(100 * time.Millisecond)

	found := false
	for _, s := range exp.GetSpans() {
		if !strings.HasPrefix(s.Name, "ws.dispatch") {
			continue
		}
		if s.SpanContext.TraceID() != parentTID {
			t.Errorf("dispatch span TraceID = %s, want parent %s",
				s.SpanContext.TraceID(), parentTID)
		}
		if s.Parent.SpanID() != parentSID {
			t.Errorf("dispatch span Parent.SpanID = %s, want %s",
				s.Parent.SpanID(), parentSID)
		}
		found = true
	}
	if !found {
		t.Fatalf("no ws.dispatch span observed; got %d spans", len(exp.GetSpans()))
	}
}
