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

// TestPreferences_LegacyCloudFlatShapeMigrates ensures a preferences.json
// written before the cloud-as-list refactor (single inline profile under
// "cloud") loads into a single "default" profile and ActiveCloud returns
// it. Guards the user's existing on-disk configs from breaking silently.
func TestPreferences_LegacyCloudFlatShapeMigrates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".clank")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `{
		"cloud": {
			"gateway_url":  "https://gw.example.com",
			"auth_url":     "https://auth.example.com",
			"access_token": "tok-legacy",
			"user_email":   "u@example.com"
		}
	}`
	if err := os.WriteFile(filepath.Join(dir, "preferences.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	prefs, err := LoadPreferences()
	if err != nil {
		t.Fatalf("LoadPreferences: %v", err)
	}
	if prefs.Remote == nil {
		t.Fatal("Cloud should not be nil after legacy migration")
	}
	if prefs.Remote.Active != "default" {
		t.Errorf("Active: got %q, want default", prefs.Remote.Active)
	}
	p := prefs.ActiveRemote()
	if p == nil {
		t.Fatal("ActiveCloud should resolve to the migrated profile")
	}
	if p.GatewayURL != "https://gw.example.com" || p.AccessToken != "tok-legacy" || p.UserEmail != "u@example.com" {
		t.Errorf("migrated profile: %+v", p)
	}
}

// TestPreferences_MultiProfileLoads verifies the new shape round-trips
// and ActiveCloud honors the Active selector.
func TestPreferences_MultiProfileLoads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".clank")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := `{
		"cloud": {
			"active": "managed",
			"profiles": {
				"dev":     {"gateway_url": "http://localhost:7878", "access_token": "dev-tok"},
				"managed": {"gateway_url": "https://api.example.com", "access_token": "prod-tok"}
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(dir, "preferences.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	prefs, err := LoadPreferences()
	if err != nil {
		t.Fatalf("LoadPreferences: %v", err)
	}
	if prefs.Remote == nil || len(prefs.Remote.Profiles) != 2 {
		t.Fatalf("expected 2 profiles, got %+v", prefs.Remote)
	}
	p := prefs.ActiveRemote()
	if p == nil || p.GatewayURL != "https://api.example.com" {
		t.Errorf("ActiveCloud should resolve to managed profile, got %+v", p)
	}
}

// TestActiveProfileAndName_Match pins the happy path: Active matches a
// profile key, both halves of the return identify the same entry.
func TestActiveProfileAndName_Match(t *testing.T) {
	t.Parallel()
	c := &RemoteConfig{
		Active: "prod",
		Profiles: map[string]*Remote{
			"prod": {GatewayURL: "https://p"},
			"dev":  {GatewayURL: "https://d"},
		},
	}
	p, name := c.ActiveProfileAndName()
	if name != "prod" {
		t.Errorf("name got %q, want %q", name, "prod")
	}
	if p == nil || p.GatewayURL != "https://p" {
		t.Errorf("profile got %+v, want prod", p)
	}
}

// TestActiveProfileAndName_FallbackAlignsName is the regression: when
// Active is empty (legacy prefs, hand-edits) but profiles exist, the
// returned name must point at the resolved profile — not at the stale
// empty Active. Persistence-side callers (login/logout/refresh) write
// under this name; mis-aligning it routes writes to "" and orphans the
// real profile.
func TestActiveProfileAndName_FallbackAlignsName(t *testing.T) {
	t.Parallel()
	c := &RemoteConfig{
		Active: "", // legacy / hand-edited
		Profiles: map[string]*Remote{
			"dev":   {GatewayURL: "https://d"},
			"alpha": {GatewayURL: "https://a"},
		},
	}
	p, name := c.ActiveProfileAndName()
	if name != "alpha" {
		t.Errorf("name got %q, want alphabetically-first %q", name, "alpha")
	}
	if p == nil || p.GatewayURL != "https://a" {
		t.Errorf("profile got %+v, want alpha", p)
	}
}

// TestActiveProfileAndName_StaleActiveFallsBack: Active points at a
// missing key — fallback fires and the name must match the resolved
// profile, not the dead Active value.
func TestActiveProfileAndName_StaleActiveFallsBack(t *testing.T) {
	t.Parallel()
	c := &RemoteConfig{
		Active: "deleted",
		Profiles: map[string]*Remote{
			"dev": {GatewayURL: "https://d"},
		},
	}
	p, name := c.ActiveProfileAndName()
	if name != "dev" {
		t.Errorf("name got %q, want %q", name, "dev")
	}
	if p == nil {
		t.Fatal("profile is nil")
	}
}

func TestActiveProfileAndName_Empty(t *testing.T) {
	t.Parallel()
	p, name := (&RemoteConfig{}).ActiveProfileAndName()
	if p != nil || name != "" {
		t.Errorf("empty config got (%v, %q), want (nil, \"\")", p, name)
	}
	p, name = (*RemoteConfig)(nil).ActiveProfileAndName()
	if p != nil || name != "" {
		t.Errorf("nil receiver got (%v, %q), want (nil, \"\")", p, name)
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
