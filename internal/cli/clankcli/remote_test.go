package clankcli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/acksell/clank/internal/config"
)

// seedPrefs writes a preferences.json with the given remote profiles into
// an isolated CLANK_DIR for the duration of the test.
func seedPrefs(t *testing.T, active string, profiles map[string]*config.Remote) {
	t.Helper()
	t.Setenv("CLANK_DIR", t.TempDir())
	if err := config.SavePreferences(config.Preferences{
		Remote: &config.RemoteConfig{Active: active, Profiles: profiles},
	}); err != nil {
		t.Fatalf("seed preferences: %v", err)
	}
}

func TestRemoteRemove_UnknownNameReturnsError(t *testing.T) {
	seedPrefs(t, "prod", map[string]*config.Remote{
		"prod": {GatewayURL: "https://gw.example.com"},
	})

	cmd := remoteRemoveCmd()
	cmd.SetArgs([]string{"typo"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for unknown remote, got nil")
	}
	if !strings.Contains(err.Error(), "no remote named") {
		t.Errorf("error %q does not mention 'no remote named'", err)
	}

	// Confirm the existing remote was untouched.
	prefs, err := config.LoadPreferences()
	if err != nil {
		t.Fatalf("reload prefs: %v", err)
	}
	if _, ok := prefs.Remote.Profiles["prod"]; !ok {
		t.Error("prod remote was unexpectedly deleted")
	}
}

func TestRemoteRemove_ActiveFallbackIsDeterministic(t *testing.T) {
	// Three peers; active is the alphabetically-last one. Removing it
	// must hand active to the alphabetically-first remaining ("bravo"),
	// not a random map-iteration pick.
	seedPrefs(t, "charlie", map[string]*config.Remote{
		"alpha":   {GatewayURL: "https://a"},
		"bravo":   {GatewayURL: "https://b"},
		"charlie": {GatewayURL: "https://c"},
	})

	cmd := remoteRemoveCmd()
	cmd.SetArgs([]string{"charlie"})
	cmd.SetOut(&bytes.Buffer{})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("remove charlie: %v", err)
	}

	prefs, err := config.LoadPreferences()
	if err != nil {
		t.Fatalf("reload prefs: %v", err)
	}
	if prefs.Remote.Active != "alpha" {
		t.Errorf("active remote after charlie removal: got %q, want %q (sorted-first)", prefs.Remote.Active, "alpha")
	}
}

func TestRemoteList_VerboseHandlesNilProfileEntry(t *testing.T) {
	// Simulate a hand-edited preferences.json that contains an explicit
	// nil profile — the verbose path must not panic dereferencing it.
	seedPrefs(t, "good", map[string]*config.Remote{
		"good":   {GatewayURL: "https://gw.example.com", UserEmail: "u@example.com"},
		"broken": nil,
	})

	var buf bytes.Buffer
	cmd := remoteCmd()
	cmd.SetArgs([]string{"-v"})
	cmd.SetOut(&buf)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("verbose list: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "broken") || !strings.Contains(out, "invalid profile") {
		t.Errorf("verbose output should flag the nil entry; got:\n%s", out)
	}
	if !strings.Contains(out, "good") || !strings.Contains(out, "u@example.com") {
		t.Errorf("verbose output should still render the good entry; got:\n%s", out)
	}
}
