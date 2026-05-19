package wsserver

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/mattcheramie/nomaddev/internal/event"
)

// b64 returns base64 encoded bytes; small helper used across the validator
// tests.
func b64(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

func TestDecodeIntentImages_EmptyIsZeroAlloc(t *testing.T) {
	got, err := decodeIntentImages(nil, 4, 1024)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got != nil {
		t.Errorf("got = %+v, want nil", got)
	}
}

func TestDecodeIntentImages_HappyPath(t *testing.T) {
	payload := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F'} // jpeg-ish bytes
	in := []event.ImageInput{
		{MediaType: "image/jpeg", Data: b64(payload)},
		{MediaType: "image/png", Data: b64([]byte("\x89PNG\r\n"))},
	}
	got, err := decodeIntentImages(in, 4, 1024)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
	if got[0].MediaType != "image/jpeg" || string(got[0].Data) != string(payload) {
		t.Errorf("got[0] = %+v", got[0])
	}
}

func TestDecodeIntentImages_TooMany(t *testing.T) {
	in := []event.ImageInput{
		{MediaType: "image/png", Data: b64([]byte("a"))},
		{MediaType: "image/png", Data: b64([]byte("b"))},
		{MediaType: "image/png", Data: b64([]byte("c"))},
	}
	_, err := decodeIntentImages(in, 2, 1024)
	if err == nil || !strings.Contains(err.Error(), "too many") {
		t.Fatalf("want too-many error, got %v", err)
	}
}

func TestDecodeIntentImages_DisabledByZeroCap(t *testing.T) {
	in := []event.ImageInput{{MediaType: "image/jpeg", Data: b64([]byte("x"))}}
	_, err := decodeIntentImages(in, 0, 1024)
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("MaxCount=0: want disabled, got %v", err)
	}
	_, err = decodeIntentImages(in, 4, 0)
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("MaxBytes=0: want disabled, got %v", err)
	}
}

func TestDecodeIntentImages_RejectsUnsupportedMediaType(t *testing.T) {
	in := []event.ImageInput{{MediaType: "application/pdf", Data: b64([]byte("%PDF"))}}
	_, err := decodeIntentImages(in, 4, 1024)
	if err == nil || !strings.Contains(err.Error(), "unsupported media_type") {
		t.Fatalf("want unsupported media_type, got %v", err)
	}
}

func TestDecodeIntentImages_RejectsBadBase64(t *testing.T) {
	in := []event.ImageInput{{MediaType: "image/jpeg", Data: "not-valid-base64!!!"}}
	_, err := decodeIntentImages(in, 4, 1024)
	if err == nil || !strings.Contains(err.Error(), "invalid base64") {
		t.Fatalf("want invalid-base64 error, got %v", err)
	}
}

func TestDecodeIntentImages_RejectsOversizedDecoded(t *testing.T) {
	huge := make([]byte, 1024)
	in := []event.ImageInput{{MediaType: "image/jpeg", Data: b64(huge)}}
	_, err := decodeIntentImages(in, 4, 512)
	if err == nil || !strings.Contains(err.Error(), "exceeds cap") {
		t.Fatalf("want size error, got %v", err)
	}
}

func TestDecodeIntentImages_RejectsEmptyPayload(t *testing.T) {
	in := []event.ImageInput{{MediaType: "image/jpeg", Data: b64([]byte{})}}
	_, err := decodeIntentImages(in, 4, 1024)
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("want empty error, got %v", err)
	}
}
