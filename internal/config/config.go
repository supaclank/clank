package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// prefsMu serializes load-modify-save updates to the preferences file so
// concurrent callers (e.g. background goroutines persisting different
// settings at once) don't clobber each other by writing back stale data.
var prefsMu sync.Mutex

// Dir returns the path to the clank configuration directory (~/.clank).
func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	return filepath.Join(home, ".clank"), nil
}

// ModelPreference stores the user's preferred model selection.
type ModelPreference struct {
	ModelID    string `json:"model_id"`
	ProviderID string `json:"provider_id"`
}

// Preferences stores user preferences that persist across sessions.
// All fields should be optional (omitempty) so the file can grow over
// time without breaking older installs.
type Preferences struct {
	Model *ModelPreference `json:"model,omitempty"`
	// ColorScheme is the TUI color scheme name (e.g. "tokyo-night").
	// Empty string means "use the default scheme".
	ColorScheme string `json:"color_scheme,omitempty"`
	// DefaultBackend is the user's preferred coding agent backend
	// (e.g. "opencode", "claude-code"). Used when neither the CLI
	// `--backend` flag nor an explicit TUI selection overrides it.
	// Empty string means "use the built-in default" (agent.DefaultBackend).
	//
	// Stored as a plain string rather than agent.BackendType to avoid
	// pulling internal/agent into the config package's dependency graph.
	// Validate at the boundary via agent.ResolveBackendPreference.
	DefaultBackend string `json:"default_backend,omitempty"`
}

// preferencesPath returns the path to the preferences file.
func preferencesPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "preferences.json"), nil
}

// LoadPreferences reads preferences from disk. Returns a zero Preferences
// (not an error) if the file doesn't exist yet.
func LoadPreferences() (Preferences, error) {
	path, err := preferencesPath()
	if err != nil {
		return Preferences{}, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Preferences{}, nil
	}
	if err != nil {
		return Preferences{}, fmt.Errorf("read preferences: %w", err)
	}
	var prefs Preferences
	if err := json.Unmarshal(data, &prefs); err != nil {
		return Preferences{}, fmt.Errorf("parse preferences: %w", err)
	}
	return prefs, nil
}

// UpdatePreferences serializes a load-modify-save against the preferences
// file. mutate is called with the most recently saved Preferences and may
// modify any subset of fields; the merged value is then written back. This
// is the safe way to change a single field from a goroutine — calling
// LoadPreferences/SavePreferences directly races other concurrent updaters.
func UpdatePreferences(mutate func(*Preferences)) error {
	prefsMu.Lock()
	defer prefsMu.Unlock()
	prefs, err := LoadPreferences()
	if err != nil {
		return err
	}
	mutate(&prefs)
	return SavePreferences(prefs)
}

// SavePreferences writes preferences to disk, creating the config directory
// if necessary.
func SavePreferences(prefs Preferences) error {
	path, err := preferencesPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(prefs, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal preferences: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write preferences: %w", err)
	}
	return nil
}
