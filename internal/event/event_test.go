package event

import (
	"errors"
	"strings"
	"testing"
)

func TestEnvelope_Roundtrip(t *testing.T) {
	in, err := NewEnvelope(EventPing, PingPayload{Nonce: "abc"})
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	b, err := in.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}

	out, err := DecodeBytes(b)
	if err != nil {
		t.Fatalf("DecodeBytes: %v", err)
	}
	if out.Type != EventPing {
		t.Errorf("Type = %q", out.Type)
	}
	if out.ID != in.ID {
		t.Errorf("ID mismatch: %q vs %q", out.ID, in.ID)
	}

	var p PingPayload
	if err := out.UnmarshalPayload(&p); err != nil {
		t.Fatalf("UnmarshalPayload: %v", err)
	}
	if p.Nonce != "abc" {
		t.Errorf("Nonce = %q", p.Nonce)
	}
}

func TestNewReply_SetsCorrelationID(t *testing.T) {
	r, err := NewReply(EventPong, "01HX-source", PingPayload{Nonce: "x"})
	if err != nil {
		t.Fatalf("NewReply: %v", err)
	}
	if r.CorrelationID != "01HX-source" {
		t.Errorf("CorrelationID = %q", r.CorrelationID)
	}
}

func TestDecode_RejectsMissingType(t *testing.T) {
	_, err := DecodeBytes([]byte(`{"id":"x","ts":"2026-01-01T00:00:00Z"}`))
	if !errors.Is(err, ErrMissingType) {
		t.Fatalf("want ErrMissingType, got %v", err)
	}
}

func TestDecode_RejectsBadJSON(t *testing.T) {
	_, err := DecodeBytes([]byte(`{not json`))
	if err == nil {
		t.Fatal("want error on bad JSON")
	}
}

func TestDecode_ReaderInterface(t *testing.T) {
	in, _ := NewEnvelope(EventHello, HelloPayload{SessionID: "s", ProtocolVersion: 1})
	b, _ := in.Bytes()

	out, err := Decode(strings.NewReader(string(b)))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if out.Type != EventHello {
		t.Errorf("Type = %q", out.Type)
	}
}

func TestNewID_Sortable(t *testing.T) {
	a := NewID()
	b := NewID()
	if a == b {
		t.Fatal("ids collided")
	}
	if !(a <= b) {
		t.Errorf("ULIDs should sort by generation order: %q vs %q", a, b)
	}
}

func TestCommandRequest_Roundtrip(t *testing.T) {
	in, err := NewEnvelope(EventCommandRequest, CommandRequestPayload{
		Tool: "execute_script",
		Args: map[string]any{"shell": "bash", "script": "echo hi"},
		// json round-trip turns int into float64 unless we re-Unmarshal into a typed payload, which we do here.
		TimeoutMs: 1500,
	})
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	b, _ := in.Bytes()
	out, err := DecodeBytes(b)
	if err != nil {
		t.Fatalf("DecodeBytes: %v", err)
	}
	var p CommandRequestPayload
	if err := out.UnmarshalPayload(&p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if p.Tool != "execute_script" || p.TimeoutMs != 1500 {
		t.Fatalf("payload mismatch: %+v", p)
	}
	if p.Args["shell"] != "bash" || p.Args["script"] != "echo hi" {
		t.Fatalf("args mismatch: %+v", p.Args)
	}
}

func TestCommandChunk_Roundtrip(t *testing.T) {
	in, err := NewReply(EventCommandChunk, "01HX-req",
		CommandChunkPayload{Stream: StreamStdout, Seq: 3, Data: "hello\n"})
	if err != nil {
		t.Fatalf("NewReply: %v", err)
	}
	b, _ := in.Bytes()
	out, _ := DecodeBytes(b)
	if out.CorrelationID != "01HX-req" {
		t.Errorf("correlation_id = %q", out.CorrelationID)
	}
	var p CommandChunkPayload
	_ = out.UnmarshalPayload(&p)
	if p.Stream != StreamStdout || p.Seq != 3 || p.Data != "hello\n" {
		t.Fatalf("chunk mismatch: %+v", p)
	}
}

func TestCommandResult_Roundtrip(t *testing.T) {
	in, _ := NewReply(EventCommandResult, "01HX-req",
		CommandResultPayload{ExitCode: -1, DurationMs: 42, Error: SandboxErrTimeout, ErrorMessage: "took too long"})
	b, _ := in.Bytes()
	out, _ := DecodeBytes(b)
	var p CommandResultPayload
	_ = out.UnmarshalPayload(&p)
	if p.ExitCode != -1 || p.Error != SandboxErrTimeout || p.DurationMs != 42 {
		t.Fatalf("result mismatch: %+v", p)
	}
}
