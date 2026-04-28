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

// Dir returns the path to the clank configuration directory (default
// ~/.clank). Can be overridden with the CLANK_DIR environment variable;
// useful for running multiple clankd instances on the same machine
// (e.g. laptop hub + remote hub for hub-to-hub sync development).
//
// A leading "~" or "~/..." in CLANK_DIR is expanded to the user's home
// directory. Without this, a literal "~/.clank-cloud" gets created as
// a relative directory in the cwd when CLANK_DIR is set by something
// that doesn't perform shell-style tilde expansion (quoted shell
// values, a launchd/systemd unit, a docker `-e`).
func Dir() (string, error) {
	if d := os.Getenv("CLANK_DIR"); d != "" {
		return expandHome(d)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	return filepath.Join(home, ".clank"), nil
}

// expandHome resolves a leading "~" or "~/..." against the current
// user's home directory. "~user" forms are intentionally not supported
// — we'd need to consult /etc/passwd which adds platform-specific
// behavior for marginal value.
func expandHome(p string) (string, error) {
	if p == "" || p[0] != '~' {
		return p, nil
	}
	if len(p) > 1 && p[1] != '/' && p[1] != filepath.Separator {
		// "~user/..." — leave unchanged so callers see the literal
		// path and can decide what to do.
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expand ~: %w", err)
	}
	if len(p) == 1 {
		return home, nil
	}
	return filepath.Join(home, p[1:]), nil
}

// ModelPreference stores the user's preferred model selection.
type ModelPreference struct {
	ModelID    string `json:"model_id"`
	ProviderID string `json:"provider_id"`
}

// RemoteHubPreference configures hub-to-hub sync. Populated symmetrically
// on both ends: the laptop hub sets URL+AuthToken to know where to push
// synced git data; a TCP-listening hub uses AuthToken to validate inbound
// bearer tokens. URL may be empty on a hub that only acts as a sync
// receiver.
type RemoteHubPreference struct {
	URL       string `json:"url,omitempty"`
	AuthToken string `json:"auth_token,omitempty"`
}

// DaytonaPreference configures the Daytona host launcher on a cloud
// hub. APIKey enables the launcher; everything else is optional and
// has sensible defaults. Forwarded into spawned sandboxes via env so
// the agent backend has the credentials it needs.
type DaytonaPreference struct {
	APIKey   string            `json:"api_key,omitempty"`
	Image    string            `json:"image,omitempty"`
	BaseURL  string            `json:"base_url,omitempty"`
	ExtraEnv map[string]string `json:"extra_env,omitempty"`
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

	// RemoteHub configures hub-to-hub sync. See RemoteHubPreference.
	RemoteHub *RemoteHubPreference `json:"remote_hub,omitempty"`

	// ActiveHub picks which hub the local TUI/CLI talks to:
	//
	//   ""        — implicit "local"; the local Unix-socket daemon.
	//   "local"   — explicit "local"; same behavior as "".
	//   "remote"  — talk to RemoteHub.URL with RemoteHub.AuthToken
	//               over TCP. Requires RemoteHub to be set.
	//
	// Used by hubclient.NewDefaultClient to pick the transport. Only
	// affects clients (TUI, clankcli); the local clankd daemon always
	// listens on its own socket regardless of this value.
	ActiveHub string `json:"active_hub,omitempty"`

	// SyncedRepos lists git RemoteURLs that the laptop sync agent will
	// push to RemoteHub. Repos not on this list are ignored — explicit
	// opt-in avoids accidental data exfiltration.
	SyncedRepos []string `json:"synced_repos,omitempty"`

	// Daytona configures the cloud-hub-side Daytona launcher. Only
	// effective on a TCP-listening hub. Empty = launcher disabled
	// (sessions requesting launch_host.provider="daytona" will 4xx).
	Daytona *DaytonaPreference `json:"daytona,omitempty"`

	// DefaultLaunchHostProvider, when set, is applied to every new
	// session whose StartRequest omits LaunchHost. Use this on a
	// cloud hub to make TUI-created sessions automatically spin up
	// sandboxes (e.g. "daytona") without each client having to know
	// about launchers.
	//
	// Empty (default) = no auto-launch; sessions land on the hub's
	// "local" host (the cloud-hub machine itself).
	//
	// Stored as a plain string to avoid importing internal/agent
	// into the config package — the value is validated at the
	// hub when a launcher is looked up.
	DefaultLaunchHostProvider string `json:"default_launch_host_provider,omitempty"`
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
