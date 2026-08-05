package github

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestStorePath(t *testing.T) {
	t.Parallel()
	s := NewStore("/home/x")
	got := s.Path()
	want := filepath.Join("/home/x", ".local", "share", "clank", "github.json")
	if got != want {
		t.Fatalf("Path() = %q, want %q", got, want)
	}
}

func TestRead_MissingFile(t *testing.T) {
	t.Parallel()
	s := NewStore(t.TempDir())
	c, err := s.Read()
	if err != nil {
		t.Fatalf("Read on missing file: %v", err)
	}
	if c.AccessToken != "" {
		t.Fatalf("expected zero Credentials, got %+v", c)
	}
	if s.IsConnected() {
		t.Fatal("IsConnected should be false when no file")
	}
}

func TestRead_EmptyFile(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	s := NewStore(home)
	if err := os.MkdirAll(filepath.Dir(s.Path()), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(s.Path(), nil, 0o600); err != nil {
		t.Fatalf("write empty: %v", err)
	}
	c, err := s.Read()
	if err != nil {
		t.Fatalf("Read on empty file: %v", err)
	}
	if c.AccessToken != "" {
		t.Fatalf("expected zero Credentials, got %+v", c)
	}
}

func TestWriteThenRead_RoundTrip(t *testing.T) {
	t.Parallel()
	s := NewStore(t.TempDir())
	installedAt := time.Date(2026, 5, 23, 10, 0, 0, 0, time.UTC)
	expiresAt := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	want := Credentials{
		AccessToken:  "gho_abc123",
		RefreshToken: "ghr_xyz789",
		ExpiresAt:    expiresAt,
		Scopes:       []string{"repo", "read:user"},
		GitHubLogin:  "octocat",
		GitHubUserID: 12345,
		InstalledAt:  installedAt,
	}
	if err := s.Write(want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := s.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.AccessToken != want.AccessToken ||
		got.RefreshToken != want.RefreshToken ||
		!got.ExpiresAt.Equal(want.ExpiresAt) ||
		!got.InstalledAt.Equal(want.InstalledAt) ||
		got.GitHubLogin != want.GitHubLogin ||
		got.GitHubUserID != want.GitHubUserID ||
		len(got.Scopes) != len(want.Scopes) {
		t.Fatalf("roundtrip mismatch:\n got=%+v\nwant=%+v", got, want)
	}
	if !s.IsConnected() {
		t.Fatal("IsConnected should be true after write")
	}
}

func TestWrite_RejectsEmptyToken(t *testing.T) {
	t.Parallel()
	s := NewStore(t.TempDir())
	if err := s.Write(Credentials{}); err == nil {
		t.Fatal("Write should reject empty access_token")
	}
}

func TestWrite_Permissions(t *testing.T) {
	t.Parallel()
	// Permission bits on the credential file matter: we never want
	// the token to be world-readable. Skip on Windows (no POSIX
	// perms), still meaningful on macOS/Linux where the sprite runs.
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions not enforced on windows")
	}
	s := NewStore(t.TempDir())
	if err := s.Write(Credentials{AccessToken: "gho_x"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("file perm = %v, want 0600", got)
	}
	dir, err := os.Stat(filepath.Dir(s.Path()))
	if err != nil {
		t.Fatalf("Stat dir: %v", err)
	}
	if got := dir.Mode().Perm(); got != 0o700 {
		t.Fatalf("dir perm = %v, want 0700", got)
	}
}

func TestWrite_NoLeakedTmp(t *testing.T) {
	t.Parallel()
	s := NewStore(t.TempDir())
	if err := s.Write(Credentials{AccessToken: "gho_x"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := os.Stat(s.Path() + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf(".tmp file should not exist after successful write")
	}
}

func TestWrite_OverwriteAtomically(t *testing.T) {
	t.Parallel()
	s := NewStore(t.TempDir())
	if err := s.Write(Credentials{AccessToken: "first"}); err != nil {
		t.Fatalf("Write first: %v", err)
	}
	if err := s.Write(Credentials{AccessToken: "second"}); err != nil {
		t.Fatalf("Write second: %v", err)
	}
	c, err := s.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if c.AccessToken != "second" {
		t.Fatalf("after overwrite want=second, got=%q", c.AccessToken)
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()
	s := NewStore(t.TempDir())
	if err := s.Write(Credentials{AccessToken: "gho_x"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := s.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(s.Path()); !os.IsNotExist(err) {
		t.Fatalf("file should be gone after Delete, stat err = %v", err)
	}
	if s.IsConnected() {
		t.Fatal("IsConnected should be false after Delete")
	}
}

func TestDelete_MissingFileIsNoOp(t *testing.T) {
	t.Parallel()
	s := NewStore(t.TempDir())
	if err := s.Delete(); err != nil {
		t.Fatalf("Delete on missing file: %v", err)
	}
}
