package state

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestPassthroughCodec_RoundTrip(t *testing.T) {
	c := PassthroughCodec{}
	plain := []byte(`{"server_url":"ws://x/ws","token":"abc"}`)
	enc, err := c.Encrypt(plain)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if !bytes.Equal(enc, plain) {
		t.Fatalf("passthrough Encrypt should not modify bytes: got %q", enc)
	}
	dec, err := c.Decrypt(enc)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(dec, plain) {
		t.Fatalf("round-trip mismatch: %q", dec)
	}
}

func TestAESGCMCodec_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := NewAESGCMCodec(filepath.Join(dir, "token.key"))
	plain := []byte(`{"server_url":"ws://x/ws","token":"abc"}`)
	enc, err := c.Encrypt(plain)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Equal(enc, plain) {
		t.Fatal("ciphertext equals plaintext — encryption did nothing")
	}
	// AES-GCM nonce is random — two encrypts of the same plaintext
	// should produce distinct ciphertexts.
	enc2, err := c.Encrypt(plain)
	if err != nil {
		t.Fatalf("second Encrypt: %v", err)
	}
	if bytes.Equal(enc, enc2) {
		t.Fatal("two encrypts produced identical ciphertext — nonce reuse")
	}
	dec, err := c.Decrypt(enc)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(dec, plain) {
		t.Fatalf("round-trip mismatch: %q", dec)
	}
}

func TestAESGCMCodec_KeyPersists(t *testing.T) {
	// Two codec instances pointing at the same key file should
	// round-trip each other's output — that's how a process restart
	// recovers a previously-saved token.
	dir := t.TempDir()
	a := NewAESGCMCodec(filepath.Join(dir, "token.key"))
	b := NewAESGCMCodec(filepath.Join(dir, "token.key"))
	plain := []byte(`hello`)
	enc, err := a.Encrypt(plain)
	if err != nil {
		t.Fatalf("a.Encrypt: %v", err)
	}
	dec, err := b.Decrypt(enc)
	if err != nil {
		t.Fatalf("b.Decrypt: %v", err)
	}
	if !bytes.Equal(dec, plain) {
		t.Fatalf("cross-instance round-trip: %q", dec)
	}
	// Key file should be 0o600.
	info, err := os.Stat(filepath.Join(dir, "token.key"))
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("key file mode = %o, want 0600", mode)
	}
}

func TestAESGCMCodec_TamperRejected(t *testing.T) {
	dir := t.TempDir()
	c := NewAESGCMCodec(filepath.Join(dir, "token.key"))
	enc, err := c.Encrypt([]byte(`hello`))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	// Flip one byte of the ciphertext (skip the nonce so we hit the
	// authenticated portion).
	enc[len(enc)-1] ^= 0xff
	if _, err := c.Decrypt(enc); err == nil {
		t.Fatal("Decrypt should reject tampered ciphertext")
	}
}

func TestAESGCMCodec_WipeForcesNewKey(t *testing.T) {
	dir := t.TempDir()
	c := NewAESGCMCodec(filepath.Join(dir, "token.key"))
	enc, err := c.Encrypt([]byte(`hello`))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if err := c.Wipe(); err != nil {
		t.Fatalf("Wipe: %v", err)
	}
	// After wipe, the codec generates a fresh key. The previous
	// ciphertext is no longer decryptable.
	if _, err := c.Decrypt(enc); err == nil {
		t.Fatal("Decrypt should fail with new key after Wipe")
	}
}

func TestAESGCMCodec_RejectsShortCiphertext(t *testing.T) {
	dir := t.TempDir()
	c := NewAESGCMCodec(filepath.Join(dir, "token.key"))
	if _, err := c.Decrypt([]byte{1, 2, 3}); err == nil {
		t.Fatal("Decrypt should reject ciphertext shorter than nonce")
	}
}
