package state

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// TokenCodec wraps a layer of encryption around the bytes written to and
// read from the token file. Implementations must round-trip:
//
//	got, _ := codec.Decrypt(codec.Encrypt(plaintext))
//	bytes.Equal(got, plaintext) == true
//
// They should also reject tampered ciphertext (i.e. provide AEAD
// guarantees) so a file edited under the user's nose surfaces as a
// decrypt error rather than a silent corruption.
type TokenCodec interface {
	Encrypt(plaintext []byte) ([]byte, error)
	Decrypt(ciphertext []byte) ([]byte, error)
}

// PassthroughCodec is the no-op codec: file contents are the raw JSON
// the M2 fileTokenStore writes. Used as the default on platforms where
// no platform keystore is wired in (desktop dev, currently iOS).
type PassthroughCodec struct{}

// Encrypt returns the plaintext unchanged.
func (PassthroughCodec) Encrypt(p []byte) ([]byte, error) { return append([]byte(nil), p...), nil }

// Decrypt returns the ciphertext unchanged.
func (PassthroughCodec) Decrypt(c []byte) ([]byte, error) { return append([]byte(nil), c...), nil }

// AESGCMCodec encrypts with AES-256-GCM. The key is loaded from a key
// file on first use; if the file doesn't exist a fresh 32-byte key is
// generated from crypto/rand and persisted with 0o600 permissions. The
// key file lives alongside the token file in the app's private data
// directory.
//
// Threat model: an attacker who only has the token file (e.g. cloud
// backup, casual filesystem-share) sees ciphertext. An attacker who
// has read access to the whole app private dir (root on the device,
// debug-bridge access to an unlocked device) reads both files and can
// decrypt. The Android Keystore-backed codec (M6.4+) closes that gap
// by binding the key to the hardware-backed keystore; AESGCMCodec is
// the defense-in-depth floor we ship today.
type AESGCMCodec struct {
	keyPath string

	mu  sync.Mutex
	key []byte // cached after first load
}

// NewAESGCMCodec returns a codec that reads its 32-byte key from
// keyPath (creating it on first use). Two codecs that point at the
// same keyPath will round-trip each other's output.
func NewAESGCMCodec(keyPath string) *AESGCMCodec {
	return &AESGCMCodec{keyPath: keyPath}
}

// loadKey returns the AES-256 key, generating + persisting one on
// first use. Subsequent calls return the cached value.
func (c *AESGCMCodec) loadKey() ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.key != nil {
		return c.key, nil
	}
	data, err := os.ReadFile(c.keyPath)
	if errors.Is(err, fs.ErrNotExist) {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("state: generate token key: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(c.keyPath), 0o700); err != nil {
			return nil, fmt.Errorf("state: mkdir token key dir: %w", err)
		}
		tmp := c.keyPath + ".tmp"
		if err := os.WriteFile(tmp, key, 0o600); err != nil {
			return nil, fmt.Errorf("state: write token key: %w", err)
		}
		if err := os.Rename(tmp, c.keyPath); err != nil {
			return nil, fmt.Errorf("state: rename token key: %w", err)
		}
		c.key = key
		return key, nil
	}
	if err != nil {
		return nil, fmt.Errorf("state: read token key: %w", err)
	}
	if len(data) != 32 {
		return nil, fmt.Errorf("state: token key file has length %d, want 32", len(data))
	}
	c.key = data
	return c.key, nil
}

// Encrypt seals plaintext with AES-256-GCM. The output layout is
// `nonce || ciphertext-with-tag` so a Decrypt call only needs the key
// file plus the on-disk bytes.
func (c *AESGCMCodec) Encrypt(plaintext []byte) ([]byte, error) {
	key, err := c.loadKey()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("state: aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("state: gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("state: gcm nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt reverses Encrypt. A failure here can mean the key file was
// rotated underneath us, the ciphertext was tampered with, or the
// file is the legacy plaintext-JSON format (in which case the caller
// transparently migrates).
func (c *AESGCMCodec) Decrypt(blob []byte) ([]byte, error) {
	key, err := c.loadKey()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("state: aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("state: gcm: %w", err)
	}
	if len(blob) < gcm.NonceSize() {
		return nil, fmt.Errorf("state: ciphertext shorter than nonce size")
	}
	nonce, ct := blob[:gcm.NonceSize()], blob[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("state: gcm open: %w", err)
	}
	return plain, nil
}

// Wipe deletes the key file and clears the in-memory cache. Sign-out
// flows call this so the next session starts with a fresh key, which
// makes any leaked-key-file scenario self-healing the next time the
// operator re-onboards.
func (c *AESGCMCodec) Wipe() error {
	c.mu.Lock()
	c.key = nil
	c.mu.Unlock()
	if err := os.Remove(c.keyPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("state: wipe token key: %w", err)
	}
	return nil
}
