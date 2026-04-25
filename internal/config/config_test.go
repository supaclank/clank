package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPreferences_RoundTrip verifies that saved preferences can be loaded
// back identically, including the new ColorScheme field.
func TestPreferences_RoundTrip(t *testing.T) {
	// Not t.Parallel: LoadPreferences/SavePreferences resolve paths from
	// $HOME; the env override is global to the process. Other tests in
	// this package that rely on the real HOME would clash.
	t.Setenv("HOME", t.TempDir())

	want := Preferences{
		Model: &ModelPreference{
			ModelID:    "claude-opus-4",
			ProviderID: "anthropic",
		},
		ColorScheme:    "tokyo-night",
		DefaultBackend: "claude-code",
	}
	if err := SavePreferences(want); err != nil {
		t.Fatalf("SavePreferences: %v", err)
	}

	got, err := LoadPreferences()
	if err != nil {
		t.Fatalf("LoadPreferences: %v", err)
	}

	if got.ColorScheme != want.ColorScheme {
		t.Errorf("ColorScheme: got %q, want %q", got.ColorScheme, want.ColorScheme)
	}
	if got.DefaultBackend != want.DefaultBackend {
		t.Errorf("DefaultBackend: got %q, want %q", got.DefaultBackend, want.DefaultBackend)
	}
	if got.Model == nil || got.Model.ModelID != want.Model.ModelID {
		t.Errorf("Model: got %+v, want %+v", got.Model, want.Model)
	}
}

// TestPreferences_LoadBackwardCompat ensures an old preferences.json that
// doesn't know about ColorScheme still loads cleanly (empty string).
func TestPreferences_LoadBackwardCompat(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Simulate a preferences file written by an older version.
	dir := filepath.Join(home, ".clank")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `{"model":{"model_id":"x","provider_id":"y"}}`
	if err := os.WriteFile(filepath.Join(dir, "preferences.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	prefs, err := LoadPreferences()
	if err != nil {
		t.Fatalf("LoadPreferences: %v", err)
	}
	if prefs.ColorScheme != "" {
		t.Errorf("expected empty ColorScheme for legacy file, got %q", prefs.ColorScheme)
	}
	if prefs.Model == nil || prefs.Model.ModelID != "x" {
		t.Errorf("Model: got %+v", prefs.Model)
	}
}

// TestPreferences_MissingFileIsZero verifies the "no file yet" path returns
// a zero-value Preferences without error. Important so a first-run TUI
// doesn't error out at startup.
func TestPreferences_MissingFileIsZero(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	prefs, err := LoadPreferences()
	if err != nil {
		t.Fatalf("LoadPreferences: %v", err)
	}
	if prefs.ColorScheme != "" || prefs.Model != nil {
		t.Errorf("expected zero Preferences, got %+v", prefs)
	}
}

// TestPreferences_OmitEmpty verifies the on-disk JSON for a default
// Preferences value is empty-ish — so the file stays small until the user
// actually customises something.
func TestPreferences_OmitEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := SavePreferences(Preferences{}); err != nil {
		t.Fatal(err)
	}
	path, err := preferencesPath()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if strings.Contains(s, "color_scheme") {
		t.Errorf("color_scheme should be omitted when empty, got: %s", s)
	}
	if strings.Contains(s, "default_backend") {
		t.Errorf("default_backend should be omitted when empty, got: %s", s)
	}
	if strings.Contains(s, "model") {
		t.Errorf("model should be omitted when nil, got: %s", s)
	}
}
