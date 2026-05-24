// Package github holds the host-side GitHub integration: the
// credential store, the device-flow runtime, the GitHub API client,
// and the orchestration that combines them for "create a PR from a
// worktree" requests.
//
// Credentials live in ~/.local/share/clank/github.json — sibling to
// the Anthropic sink in internal/host/auth.go. The credential never
// travels through clank's infrastructure: the device flow runs
// between this process and github.com directly, and PR creation
// calls the GitHub API from this process too. The gateway is a pure
// proxy for the connect-flow status and PR creation requests.
package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Credentials is the on-disk shape at Store.Path(). Only AccessToken
// is required; the other fields are best-effort metadata captured
// when the device flow completes so the UI can show "@login" without
// a round-trip to GitHub.
type Credentials struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitzero"`
	Scopes       []string  `json:"scopes,omitempty"`
	GitHubLogin  string    `json:"github_login,omitempty"`
	GitHubUserID int64     `json:"github_user_id,omitempty"`
	InstalledAt  time.Time `json:"installed_at,omitzero"`
}

// Store reads and writes Credentials atomically. Safe for concurrent
// use; in practice only one writer ever exists per host (the device
// flow's polling goroutine).
type Store struct {
	homeDir string
	mu      sync.Mutex
}

// NewStore constructs a Store rooted at homeDir. The credential file
// lives at <homeDir>/.local/share/clank/github.json.
func NewStore(homeDir string) *Store {
	return &Store{homeDir: homeDir}
}

// Path returns the absolute path of the credential file.
func (s *Store) Path() string {
	return filepath.Join(s.homeDir, ".local", "share", "clank", "github.json")
}

// Read returns the persisted credentials. A missing file is treated
// as "not connected" — returns a zero Credentials with nil error so
// callers can use IsConnected to branch without an error check.
func (s *Store) Read() (Credentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readLocked()
}

func (s *Store) readLocked() (Credentials, error) {
	data, err := os.ReadFile(s.Path())
	if err != nil {
		// Treat a missing file as "not connected", but propagate
		// real errors (EACCES, EIO, ...) so the UI doesn't render
		// "Connect GitHub" when the actual problem is a permission
		// fault on an existing credential file.
		if errors.Is(err, os.ErrNotExist) {
			return Credentials{}, nil
		}
		return Credentials{}, fmt.Errorf("read github credentials: %w", err)
	}
	if len(data) == 0 {
		return Credentials{}, nil
	}
	var c Credentials
	if err := json.Unmarshal(data, &c); err != nil {
		return Credentials{}, fmt.Errorf("decode github credentials: %w", err)
	}
	return c, nil
}

// IsConnected reports whether a usable access token is stored.
// Convenience wrapper around Read for callers that don't care about
// the rest of the credential.
func (s *Store) IsConnected() bool {
	c, err := s.Read()
	if err != nil {
		return false
	}
	return c.AccessToken != ""
}

// Write replaces the credential atomically: write a sibling .tmp,
// then rename. A reader observing the file at any instant sees
// either the previous credential or the new one — never partial JSON.
func (s *Store) Write(c Credentials) error {
	if c.AccessToken == "" {
		return fmt.Errorf("github store: access_token is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.persistLocked(c)
}

func (s *Store) persistLocked(c Credentials) error {
	path := s.Path()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create github credentials dir: %w", err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode github credentials: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write tmp github credentials: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename github credentials: %w", err)
	}
	return nil
}

// Delete removes the credential file. Missing file is not an error
// — disconnect is idempotent so the UI doesn't have to think about
// "I already disconnected" races.
func (s *Store) Delete() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.Path()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove github credentials: %w", err)
	}
	return nil
}
