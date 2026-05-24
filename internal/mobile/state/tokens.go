package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// TokenStore persists the server URL + JWT across app launches.
//
// Implementations:
//   - fileTokenStore — plain JSON on disk (M2, kept for desktop dev).
//   - encryptedFileStore — AEAD codec wrapped around the same path
//     (M6.3, default on real devices).
//
// Future implementations (Android-Keystore-backed, iOS-Keychain-backed)
// satisfy the same interface so callers do not change.
type TokenStore interface {
	Load() (serverURL, token string, err error)
	Save(serverURL, token string) error
	Clear() error
}

// ErrNoToken is returned by Load when no credentials have been saved yet —
// the Onboard screen reads this as "show the empty form".
var ErrNoToken = errors.New("state: no saved token")

type fileTokenStore struct {
	path string
}

// NewFileTokenStore returns a TokenStore that reads and writes one JSON
// file at the given absolute path. The parent directory is created on
// first Save with 0o700 permissions; the file itself is written with 0o600.
func NewFileTokenStore(path string) TokenStore {
	return &fileTokenStore{path: path}
}

type tokenFile struct {
	ServerURL string `json:"server_url"`
	Token     string `json:"token"`
}

func (s *fileTokenStore) Load() (string, string, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", "", ErrNoToken
		}
		return "", "", fmt.Errorf("state: read token: %w", err)
	}
	var f tokenFile
	if err := json.Unmarshal(data, &f); err != nil {
		return "", "", fmt.Errorf("state: decode token: %w", err)
	}
	if f.Token == "" {
		return "", "", ErrNoToken
	}
	return f.ServerURL, f.Token, nil
}

func (s *fileTokenStore) Save(serverURL, token string) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("state: mkdir token dir: %w", err)
	}
	data, err := json.Marshal(tokenFile{ServerURL: serverURL, Token: token})
	if err != nil {
		return fmt.Errorf("state: encode token: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("state: write token: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("state: rename token: %w", err)
	}
	return nil
}

func (s *fileTokenStore) Clear() error {
	if err := os.Remove(s.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("state: clear token: %w", err)
	}
	return nil
}

// encryptedFileStore writes the same tokenFile JSON shape as
// fileTokenStore but runs the bytes through a TokenCodec first. The
// codec round-trips at-rest contents: an attacker reading the file
// alone gets ciphertext.
//
// Load auto-migrates from the legacy plaintext-JSON layout: if the
// first decode attempt sees `{` (the JSON object opener) we treat the
// file as plaintext, parse it directly, and rewrite encrypted on the
// next Save. This means a v0.x install that already has the M2 file
// keeps working with no manual migration.
type encryptedFileStore struct {
	path  string
	codec TokenCodec
}

// NewEncryptedFileTokenStore returns a TokenStore that wraps codec
// around a file at the given absolute path. Pass NewAESGCMCodec for the
// default at-rest encryption.
func NewEncryptedFileTokenStore(path string, codec TokenCodec) TokenStore {
	if codec == nil {
		codec = PassthroughCodec{}
	}
	return &encryptedFileStore{path: path, codec: codec}
}

func (s *encryptedFileStore) Load() (string, string, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", "", ErrNoToken
		}
		return "", "", fmt.Errorf("state: read token: %w", err)
	}
	plain := raw
	if !looksLikePlainJSON(raw) {
		decoded, err := s.codec.Decrypt(raw)
		if err != nil {
			return "", "", fmt.Errorf("state: decrypt token: %w", err)
		}
		plain = decoded
	}
	var f tokenFile
	if err := json.Unmarshal(plain, &f); err != nil {
		return "", "", fmt.Errorf("state: decode token: %w", err)
	}
	if f.Token == "" {
		return "", "", ErrNoToken
	}
	return f.ServerURL, f.Token, nil
}

func (s *encryptedFileStore) Save(serverURL, token string) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("state: mkdir token dir: %w", err)
	}
	plain, err := json.Marshal(tokenFile{ServerURL: serverURL, Token: token})
	if err != nil {
		return fmt.Errorf("state: encode token: %w", err)
	}
	enc, err := s.codec.Encrypt(plain)
	if err != nil {
		return fmt.Errorf("state: encrypt token: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, enc, 0o600); err != nil {
		return fmt.Errorf("state: write token: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("state: rename token: %w", err)
	}
	return nil
}

func (s *encryptedFileStore) Clear() error {
	if err := os.Remove(s.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("state: clear token: %w", err)
	}
	// Wipe the per-install key too so the next sign-in starts fresh —
	// the codec only exposes Wipe when the implementation supports it
	// (PassthroughCodec doesn't need a wipe).
	if w, ok := s.codec.(interface{ Wipe() error }); ok {
		if err := w.Wipe(); err != nil {
			return err
		}
	}
	return nil
}

// looksLikePlainJSON returns true when raw looks like the legacy
// fileTokenStore output — an opening brace after optional whitespace.
// Used by the encryptedFileStore to auto-migrate the M2 layout on first
// Load. AES-GCM ciphertext starts with random nonce bytes so the chance
// of a false positive (real ciphertext that begins with `{`) is
// negligible — the JSON parser following the brace check rejects
// anything that isn't actually valid JSON.
func looksLikePlainJSON(raw []byte) bool {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		case '{':
			return json.Valid(raw) && bytes.Contains(raw, []byte(`"token"`))
		default:
			return false
		}
	}
	return false
}
