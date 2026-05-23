package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// TokenStore persists the server URL + JWT across app launches. Phase M2
// uses a plain JSON file in the app's private data directory; Phase M6
// replaces this implementation with an Android-Keystore-backed AES-GCM
// store that satisfies the same interface, so callers do not change.
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
