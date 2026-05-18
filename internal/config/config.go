package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

// DaytonaPreference configures the Daytona host launcher on a cloud
// hub. APIKey enables the launcher; everything else is optional and
// has sensible defaults. Forwarded into spawned sandboxes via env so
// the agent backend has the credentials it needs.
type DaytonaPreference struct {
	APIKey string `json:"api_key,omitempty"`
	// Snapshot is a Daytona-side snapshot name (created via `daytona
	// snapshot create`). When set, sandboxes are spawned from the
	// pre-warmed snapshot and boot in ~hundreds of ms vs. seconds for
	// cold OCI image pulls. Takes precedence over Image.
	Snapshot string            `json:"snapshot,omitempty"`
	Image    string            `json:"image,omitempty"`
	BaseURL  string            `json:"base_url,omitempty"`
	ExtraEnv map[string]string `json:"extra_env,omitempty"`

	// SuspendOnStop, when true, asks the daemon to suspend the
	// persistent sandbox on shutdown (Daytona Stop) so it stops
	// billing for compute. Default false: leaves the sandbox
	// running for zero cold-resume latency on the next start.
	// Daytona bills per-second and a quick laptop reboot costs
	// cents, so the default favors latency over cost.
	SuspendOnStop bool `json:"suspend_on_stop,omitempty"`
}

// FlyIOPreference configures the Fly.io Sprites host launcher.
// APIToken (a SPRITES_TOKEN) enables the launcher; everything else
// is optional with sensible defaults.
//
// Sprites are persistent per-user — one sprite is created the first
// time EnsureHost runs and reused indefinitely. The sprite's public
// URL is set to "public" auth mode; clank-host's bearer-token
// middleware (see PR 2) is the only auth gate.
type FlyIOPreference struct {
	APIToken         string `json:"api_token,omitempty"`
	OrganizationSlug string `json:"organization_slug,omitempty"`
	Region           string `json:"region,omitempty"`
	// SpriteNamePrefix is prepended to the user identifier to form
	// the sprite name. Empty defaults to "clank-host" (yielding e.g.
	// "clank-host-local" in the single-user laptop daemon).
	SpriteNamePrefix string `json:"sprite_name_prefix,omitempty"`
	// Resource pins for the sprite. 0 uses Sprites' defaults.
	RamMB     int `json:"ram_mb,omitempty"`
	CPUs      int `json:"cpus,omitempty"`
	StorageGB int `json:"storage_gb,omitempty"`
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

	// SidebarWidthRatio is the sidebar width as a percentage of the terminal
	// width, adjusted with +/- in the TUI. Zero means "use the built-in
	// default" (defaultSidebarWidthRatio).
	SidebarWidthRatio int `json:"sidebar_width_ratio,omitempty"`

	// Daytona configures the cloud-hub-side Daytona launcher. Only
	// effective on a TCP-listening hub. Empty = launcher disabled
	// (sessions requesting launch_host.provider="daytona" will 4xx).
	Daytona *DaytonaPreference `json:"daytona,omitempty"`

	// FlyIO configures the cloud-hub-side Fly.io Sprites launcher.
	// Only effective on a TCP-listening hub. Empty = launcher
	// disabled (sessions requesting launch_host.provider="flyio"
	// will 4xx).
	FlyIO *FlyIOPreference `json:"flyio,omitempty"`

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

	// Remote configures the user's named clank deployments. One or more
	// remotes, each with its own gateway/auth endpoint and session, plus
	// an Active selector pointing at the live one. Modeled on git
	// remotes: same mental model, same `add/list/switch/remove` UX.
	// clank push/pull, the TUI auth panel, and `clank remote` all read
	// the active remote via Preferences.ActiveRemote().
	Remote *RemoteConfig `json:"remote,omitempty"`
}

// RemoteConfig holds one or more named clank deployments plus the
// Active selector. Lets the user switch between e.g. a dev docker
// stack, a managed cloud, and an enterprise self-hosted instance
// without rewriting preferences.
//
// JSON marshalling auto-detects the legacy flat shape (single profile
// inline under "cloud" or "remote") and normalizes to the multi-profile
// shape on load — saves rewrite to the new shape on the next
// SavePreferences.
type RemoteConfig struct {
	// Active is the key in Profiles whose endpoints/session are used by
	// push/pull/TUI right now. Empty falls back to "default".
	Active string `json:"active,omitempty"`

	// Profiles maps a user-chosen name to its configuration. At least
	// one entry is expected when Remote is set at all; an Active that
	// points at a missing entry renders ActiveRemote() nil.
	Profiles map[string]*Remote `json:"profiles,omitempty"`
}

// Remote holds one clank deployment's gateway URL + OAuth session.
// Mirrors a single entry in a git-remote-style config.
//
// Provider-agnostic on purpose: the gateway exposes /auth-config and
// clank runs standards OAuth 2.0 + PKCE against the IdP it advertises.
// The deployment (hosted or self-hosted) owns the user-auth mechanism
// — Supabase OAuth Server, Auth0, Keycloak, whatever. clank only
// needs one URL: the gateway.
//
// Session fields are populated after a successful OAuth grant and
// used for subsequent /me and sync calls. AccessToken expires; the
// user is prompted to sign in again on 401.
type Remote struct {
	// GatewayURL is the base URL of the cloud gateway (sessions + sync),
	// e.g. "https://gateway.example.com". Required for push/pull and
	// session proxying; also the discovery endpoint for OAuth via
	// GET /auth-config.
	GatewayURL string `json:"gateway_url,omitempty"`

	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	UserEmail    string `json:"user_email,omitempty"`
	UserID       string `json:"user_id,omitempty"`
	// ExpiresAt is unix-seconds. Zero when no session.
	ExpiresAt int64 `json:"expires_at,omitempty"`
}

// UnmarshalJSON accepts both the multi-profile shape and the legacy
// single-profile flat shape. Legacy gets normalized to a single
// "default" entry selected as Active.
func (c *RemoteConfig) UnmarshalJSON(data []byte) error {
	// Multi-profile shape first.
	type alias struct {
		Active   string             `json:"active"`
		Profiles map[string]*Remote `json:"profiles"`
	}
	var newShape alias
	if err := json.Unmarshal(data, &newShape); err == nil && len(newShape.Profiles) > 0 {
		c.Active = newShape.Active
		c.Profiles = newShape.Profiles
		if c.Active == "" {
			// Pick a deterministic default so callers don't randomly
			// resolve to different profiles between runs.
			c.Active = firstRemoteName(c.Profiles)
		}
		return nil
	}
	// Legacy flat shape. Tolerate Profiles being absent and trust the
	// inline fields. Empty object {} also lands here and yields no
	// active profile.
	var legacy Remote
	if err := json.Unmarshal(data, &legacy); err != nil {
		return err
	}
	if isZeroRemote(legacy) {
		c.Active = ""
		c.Profiles = nil
		return nil
	}
	c.Active = "default"
	c.Profiles = map[string]*Remote{"default": &legacy}
	return nil
}

// ActiveProfile returns the active Remote or nil if none.
func (c *RemoteConfig) ActiveProfile() *Remote {
	if c == nil || len(c.Profiles) == 0 {
		return nil
	}
	if p, ok := c.Profiles[c.Active]; ok {
		return p
	}
	return c.Profiles[firstRemoteName(c.Profiles)]
}

// ActiveRemote is a Preferences-level convenience for the very common
// "what's the live remote" check. Returns nil if Remote or its Active
// entry is unset.
func (p *Preferences) ActiveRemote() *Remote {
	if p == nil || p.Remote == nil {
		return nil
	}
	return p.Remote.ActiveProfile()
}

func firstRemoteName(m map[string]*Remote) string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	if len(names) == 0 {
		return ""
	}
	// Sort for determinism. Single-profile case is the common one;
	// O(n log n) on a tiny map is fine.
	sort.Strings(names)
	return names[0]
}

func isZeroRemote(p Remote) bool {
	return p == Remote{}
}

// UnmarshalJSON on Preferences migrates the legacy top-level "cloud"
// key to "remote" when the new key is absent. The next SavePreferences
// emits only "remote", quietly upgrading the file.
func (p *Preferences) UnmarshalJSON(data []byte) error {
	type alias Preferences
	aux := struct {
		*alias
		LegacyCloud *RemoteConfig `json:"cloud,omitempty"`
	}{alias: (*alias)(p)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if p.Remote == nil && aux.LegacyCloud != nil {
		p.Remote = aux.LegacyCloud
	}
	return nil
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
