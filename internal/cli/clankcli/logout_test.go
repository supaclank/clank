package clankcli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/acksell/clank/internal/config"
)

func TestLogout_ClearsSessionKeepsGateway(t *testing.T) {
	seedPrefs(t, "dev", map[string]*config.Remote{
		"dev": {
			GatewayURL:   "https://gw.example.com",
			AccessToken:  "at",
			RefreshToken: "rt",
			UserEmail:    "user@example.com",
			UserID:       "user-123",
			ExpiresAt:    1700000000,
		},
	})

	cmd := logoutCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if !strings.Contains(stdout.String(), `Signed out of remote "dev"`) {
		t.Errorf("expected 'Signed out of remote \"dev\"'; got %q", stdout.String())
	}

	prefs, err := config.LoadPreferences()
	if err != nil {
		t.Fatalf("reload prefs: %v", err)
	}
	r := prefs.Remote.Profiles["dev"]
	if r == nil {
		t.Fatal("dev remote was unexpectedly removed (logout should only clear the session)")
	}
	if r.GatewayURL != "https://gw.example.com" {
		t.Errorf("GatewayURL got %q, want preserved", r.GatewayURL)
	}
	if r.AccessToken != "" || r.RefreshToken != "" || r.UserEmail != "" || r.UserID != "" || r.ExpiresAt != 0 {
		t.Errorf("session fields not cleared: %+v", r)
	}
}

func TestLogout_TargetsActiveByDefault(t *testing.T) {
	// 'prod' is active; 'dev' is also configured. Bare `clank logout`
	// must clear prod, leave dev untouched.
	seedPrefs(t, "prod", map[string]*config.Remote{
		"prod": {GatewayURL: "https://p", AccessToken: "p-at", RefreshToken: "p-rt", ExpiresAt: 1700000000},
		"dev":  {GatewayURL: "https://d", AccessToken: "d-at", RefreshToken: "d-rt", ExpiresAt: 1700000000},
	})

	cmd := logoutCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("logout: %v", err)
	}

	prefs, _ := config.LoadPreferences()
	if prefs.Remote.Profiles["prod"].AccessToken != "" {
		t.Error("active remote 'prod' not cleared")
	}
	if prefs.Remote.Profiles["dev"].AccessToken == "" {
		t.Error("non-active remote 'dev' was unexpectedly cleared")
	}
}

func TestLogout_NamedRemote(t *testing.T) {
	seedPrefs(t, "prod", map[string]*config.Remote{
		"prod": {GatewayURL: "https://p", AccessToken: "p-at", RefreshToken: "p-rt", ExpiresAt: 1700000000},
		"dev":  {GatewayURL: "https://d", AccessToken: "d-at", RefreshToken: "d-rt", ExpiresAt: 1700000000},
	})

	cmd := logoutCmd()
	cmd.SetArgs([]string{"--remote", "dev"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("logout --remote dev: %v", err)
	}

	prefs, _ := config.LoadPreferences()
	if prefs.Remote.Profiles["dev"].AccessToken != "" {
		t.Error("named remote 'dev' not cleared")
	}
	if prefs.Remote.Profiles["prod"].AccessToken == "" {
		t.Error("active remote 'prod' was unexpectedly cleared")
	}
}

func TestLogout_UnknownRemote(t *testing.T) {
	seedPrefs(t, "prod", map[string]*config.Remote{
		"prod": {GatewayURL: "https://p", AccessToken: "p-at"},
	})

	cmd := logoutCmd()
	cmd.SetArgs([]string{"--remote", "typo"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for unknown remote, got nil")
	}
	if !strings.Contains(err.Error(), "no remote named") {
		t.Errorf("error %q should mention 'no remote named'", err)
	}
}

func TestLogout_NoActiveRemote(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir())
	cmd := logoutCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when no active remote configured, got nil")
	}
	if !strings.Contains(err.Error(), "no active remote") {
		t.Errorf("error %q should mention 'no active remote'", err)
	}
}

func TestLogout_AlreadySignedOut_Idempotent(t *testing.T) {
	seedPrefs(t, "dev", map[string]*config.Remote{
		"dev": {GatewayURL: "https://gw.example.com"}, // no session fields
	})

	cmd := logoutCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("logout should be idempotent: %v", err)
	}
	if !strings.Contains(stdout.String(), "not signed in to remote") {
		t.Errorf("expected idempotent notice; got %q", stdout.String())
	}
}
