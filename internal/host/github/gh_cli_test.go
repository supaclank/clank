package github

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// putFakeGh installs an executable `gh` stub at the FRONT of PATH so
// exec.LookPath resolves it ahead of any real gh on the machine —
// which is also what keeps these tests hermetic on a dev laptop with
// a logged-in gh.
func putFakeGh(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestAccessToken_GhCLIFallback(t *testing.T) {
	// Not parallel: t.Setenv (HOME + PATH).
	t.Setenv("HOME", t.TempDir())
	putFakeGh(t, `[ "$1 $2" = "auth token" ] && { echo gho_fromcli; exit 0; }; exit 1`)

	m := NewManager(os.Getenv("HOME"), "")

	// Fallback disabled (the default): a logged-in gh must NOT leak in.
	if _, err := m.AccessToken(); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("disabled fallback: err = %v, want ErrNotConnected", err)
	}

	m.EnableGhCLIFallback()
	token, err := m.AccessToken()
	if err != nil {
		t.Fatalf("AccessToken with gh fallback: %v", err)
	}
	if token != "gho_fromcli" {
		t.Errorf("token = %q, want gho_fromcli", token)
	}
	// Identity isn't part of gh's handoff.
	if login := m.StoredLogin(); login != "" {
		t.Errorf("login = %q, want empty for gh-borrowed token", login)
	}

	st, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Connected || st.Source != CredentialSourceGhCLI {
		t.Errorf("status = %+v, want connected via %s", st, CredentialSourceGhCLI)
	}
}

func TestAccessToken_StoreWinsOverGhCLI(t *testing.T) {
	// Not parallel: t.Setenv (HOME + PATH).
	t.Setenv("HOME", t.TempDir())
	putFakeGh(t, `[ "$1 $2" = "auth token" ] && { echo gho_fromcli; exit 0; }; exit 1`)

	m := NewManager(os.Getenv("HOME"), "")
	m.EnableGhCLIFallback()
	if err := m.Store().Write(Credentials{AccessToken: "gho_fromstore", GitHubLogin: "acksell"}); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	token, err := m.AccessToken()
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if token != "gho_fromstore" {
		t.Errorf("token = %q, want the explicit clank connection to win", token)
	}
	if login := m.StoredLogin(); login != "acksell" {
		t.Errorf("login = %q, want acksell", login)
	}
	st, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Connected || st.Source != CredentialSourceStore {
		t.Errorf("status = %+v, want connected via %s", st, CredentialSourceStore)
	}
}

func TestAccessToken_GhCLILoggedOut(t *testing.T) {
	// Not parallel: t.Setenv (HOME + PATH).
	t.Setenv("HOME", t.TempDir())
	// gh's real logged-out behavior: message on stderr, exit 1.
	putFakeGh(t, `echo "no oauth token" >&2; exit 1`)

	m := NewManager(os.Getenv("HOME"), "")
	m.EnableGhCLIFallback()
	if _, err := m.AccessToken(); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("err = %v, want ErrNotConnected when gh is logged out", err)
	}
	st, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Connected || st.Source != "" {
		t.Errorf("status = %+v, want disconnected", st)
	}
}

func TestGhCLIToken_RejectsUnusableOutput(t *testing.T) {
	// Not parallel: t.Setenv (PATH).
	cases := []struct{ name, script string }{
		{"empty output", `exit 0`},
		{"multiline output", `printf "gho_x\ngarbage\n"; exit 0`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			putFakeGh(t, tc.script)
			if got := ghCLIToken(); got != "" {
				t.Errorf("ghCLIToken() = %q, want empty for %s", got, tc.name)
			}
		})
	}
}
