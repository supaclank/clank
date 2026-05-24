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
		Models: map[string]ModelPreference{
			"claude-code": {
				ModelID:    "opus",
				ProviderID: "anthropic-claude-code",
			},
			"opencode": {
				ModelID:    "claude-opus-4",
				ProviderID: "anthropic",
			},
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
	if got.ModelFor("claude-code") != want.Models["claude-code"] {
		t.Errorf("Models[claude-code]: got %+v, want %+v", got.ModelFor("claude-code"), want.Models["claude-code"])
	}
	if got.ModelFor("opencode") != want.Models["opencode"] {
		t.Errorf("Models[opencode]: got %+v, want %+v", got.ModelFor("opencode"), want.Models["opencode"])
	}
}

// expand state survives a save/load cycle. The keys mirror the runtime
// scheme (worktree path, older buckets); the value semantics aren't
// asserted here — just that the JSON shape round-trips.
func TestPreferences_SidebarExpandedRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	want := Preferences{
		SidebarExpanded: map[string]bool{
			"wt:/home/u/repos/clank":         true,
			"wt:/home/u/repos/mindmouth":     false,
			"older:wt":                       true,
			"older:s:/home/u/repos/fuselage": true,
		},
	}
	if err := SavePreferences(want); err != nil {
		t.Fatalf("SavePreferences: %v", err)
	}

	got, err := LoadPreferences()
	if err != nil {
		t.Fatalf("LoadPreferences: %v", err)
	}

	if len(got.SidebarExpanded) != len(want.SidebarExpanded) {
		t.Fatalf("SidebarExpanded length: got %d, want %d", len(got.SidebarExpanded), len(want.SidebarExpanded))
	}
	for k, v := range want.SidebarExpanded {
		if got.SidebarExpanded[k] != v {
			t.Errorf("SidebarExpanded[%q]: got %v, want %v", k, got.SidebarExpanded[k], v)
		}
	}
}

// TestPreferences_LoadIgnoresUnknownFields guards against a strict
// unmarshal regression: preferences.json may carry fields written by a
// newer clank (or fields removed in a refactor); load must tolerate
// them so a downgrade or a stale file doesn't brick the TUI.
func TestPreferences_LoadIgnoresUnknownFields(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := filepath.Join(home, ".clank")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	withExtras := `{"color_scheme":"gruvbox-dark","unknown_future_field":42}`
	if err := os.WriteFile(filepath.Join(dir, "preferences.json"), []byte(withExtras), 0o644); err != nil {
		t.Fatal(err)
	}

	prefs, err := LoadPreferences()
	if err != nil {
		t.Fatalf("LoadPreferences: %v", err)
	}
	if prefs.ColorScheme != "gruvbox-dark" {
		t.Errorf("ColorScheme: got %q, want gruvbox-dark", prefs.ColorScheme)
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

// TestPreferences_LegacyModelMigratesToPerBackend ensures a preferences.json
// written before the per-backend split (single inline ModelPreference under
// "model") loads into Models keyed by the legacy migration target so the
// user's selection isn't silently dropped on the next SavePreferences.
// Regression: without this, every upgrading user's model selection would
// quietly disappear on the first picker write that triggered a re-save.
func TestPreferences_LegacyModelMigratesToPerBackend(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".clank")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Pin the migration target by setting DefaultBackend so the test is
	// independent of the package-level fallback.
	legacy := `{
		"default_backend": "claude-code",
		"model": {
			"model_id": "opus",
			"provider_id": "anthropic-claude-code"
		}
	}`
	if err := os.WriteFile(filepath.Join(dir, "preferences.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	prefs, err := LoadPreferences()
	if err != nil {
		t.Fatalf("LoadPreferences: %v", err)
	}
	got := prefs.ModelFor("claude-code")
	want := ModelPreference{ModelID: "opus", ProviderID: "anthropic-claude-code"}
	if got != want {
		t.Errorf("ModelFor(claude-code): got %+v, want %+v", got, want)
	}
}

// TestPreferences_LegacyModelMigratesToFallbackWhenDefaultBackendEmpty
// pins the no-DefaultBackend behaviour: the legacy single pref still
// migrates, falling back to the package default backend key so the user's
// data isn't dropped just because they hadn't explicitly chosen a default.
func TestPreferences_LegacyModelMigratesToFallbackWhenDefaultBackendEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".clank")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `{
		"model": {
			"model_id": "claude-opus-4",
			"provider_id": "anthropic"
		}
	}`
	if err := os.WriteFile(filepath.Join(dir, "preferences.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	prefs, err := LoadPreferences()
	if err != nil {
		t.Fatalf("LoadPreferences: %v", err)
	}
	got := prefs.ModelFor(legacyModelMigrationBackend)
	want := ModelPreference{ModelID: "claude-opus-4", ProviderID: "anthropic"}
	if got != want {
		t.Errorf("ModelFor(%s): got %+v, want %+v", legacyModelMigrationBackend, got, want)
	}
}

// TestPreferences_NewModelsTakesPrecedenceOverLegacy ensures the migration
// is non-destructive when a user's preferences.json contains both shapes
// (e.g. a partial hand-edit, or a downgrade-then-upgrade cycle): the new
// Models map wins and the legacy "model" is ignored.
func TestPreferences_NewModelsTakesPrecedenceOverLegacy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".clank")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mixed := `{
		"default_backend": "claude-code",
		"model":  { "model_id": "old-id",   "provider_id": "old-prov" },
		"models": { "claude-code": { "model_id": "new-id", "provider_id": "new-prov" } }
	}`
	if err := os.WriteFile(filepath.Join(dir, "preferences.json"), []byte(mixed), 0o644); err != nil {
		t.Fatal(err)
	}
	prefs, err := LoadPreferences()
	if err != nil {
		t.Fatalf("LoadPreferences: %v", err)
	}
	got := prefs.ModelFor("claude-code")
	want := ModelPreference{ModelID: "new-id", ProviderID: "new-prov"}
	if got != want {
		t.Errorf("ModelFor(claude-code): got %+v, want %+v (new shape must win)", got, want)
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
	if prefs.ColorScheme != "" || len(prefs.Models) != 0 {
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
