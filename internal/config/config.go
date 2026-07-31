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
		expanded, err := expandHome(d)
		if err != nil {
			return "", err
		}
		// Absolute: a relative CLANK_DIR resolves against whatever cwd
		// happens to be current, which differs between callers (e.g. a
		// preview's spawned shell runs with cwd=workDir) and silently
		// splits one logical state dir into several on-disk locations.
		abs, err := filepath.Abs(expanded)
		if err != nil {
			return "", fmt.Errorf("resolve CLANK_DIR %q: %w", d, err)
		}
		return abs, nil
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

// IsZero reports whether the preference is unset.
func (m ModelPreference) IsZero() bool {
	return m.ModelID == "" && m.ProviderID == ""
}

// FlySpritesPreference configures the Fly.io Sprites host launcher.
// APIToken (a SPRITES_TOKEN) enables the launcher; everything else
// is optional with sensible defaults.
//
// Sprites are persistent per-user — one sprite is created the first
// time EnsureHost runs and reused indefinitely. The sprite's public
// URL is set to "public" auth mode; clank-host's bearer-token
// middleware (see PR 2) is the only auth gate.
type FlySpritesPreference struct {
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

// FlyMachinesPreference configures the raw Fly Machines provisioner
// (pkg/provisioner/flymachines): one app+machine+volume per user.
// Required: api_token (org-scoped, app-create rights), org_slug,
// region, and image (the clank-host-fly OCI ref, ideally digest-
// pinned). Zero-valued sizing fields use the provisioner defaults.
type FlyMachinesPreference struct {
	APIToken string `json:"api_token,omitempty"`
	OrgSlug  string `json:"org_slug,omitempty"`
	Region   string `json:"region,omitempty"`
	Image    string `json:"image,omitempty"`
	// GatewayNetwork is the Fly private network the per-app Flycast
	// address is allocated on — where this daemon dials FROM. Empty
	// means the org's default network.
	GatewayNetwork string `json:"gateway_network,omitempty"`
	// AppNamePrefix namespaces this deployment's per-user apps within
	// the org (e.g. "clank-dev-u"). Empty defaults to "clank-u".
	AppNamePrefix string `json:"app_name_prefix,omitempty"`
	GuestCPUKind  string `json:"guest_cpu_kind,omitempty"`
	GuestCPUs     int    `json:"guest_cpus,omitempty"`
	GuestMemoryMB int    `json:"guest_memory_mb,omitempty"`
	SwapSizeMB    int    `json:"swap_size_mb,omitempty"`
	VolumeSizeGB  int    `json:"volume_size_gb,omitempty"`
}

// Preferences stores user preferences that persist across sessions.
// All fields should be optional (omitempty) so the file can grow over
// time without breaking older installs.
type Preferences struct {
	// Models holds per-backend model overrides, keyed by backend
	// string (e.g. "opencode", "claude-code"). Per-backend rather
	// than a single global pick because each backend has its own
	// model catalog: a model valid for opencode (e.g. a github-copilot
	// route) will not exist in claude-code's enum and would crash the
	// CLI at spawn time. Use ModelFor/SetModelFor.
	Models map[string]ModelPreference `json:"models,omitempty"`
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

	// WebPreviewDictation is the dictation engine `clank preview`'s
	// browser overlay uses: "local" (the clank-voice/exec engine on
	// this machine) or "webspeech" (the browser's SpeechRecognition
	// service, which typically uploads audio to the browser vendor).
	// Empty means not chosen yet — the overlay asks on first dictation.
	//
	// Stored as a plain string (same reasoning as DefaultBackend);
	// validated at the boundary via webpreview.ParseDictationEngine.
	WebPreviewDictation string `json:"web_preview_dictation,omitempty"`

	// SidebarWidthRatio is the sidebar width as a percentage of the terminal
	// width, adjusted with +/- in the TUI. Zero means "use the built-in
	// default" (defaultSidebarWidthRatio).
	SidebarWidthRatio int `json:"sidebar_width_ratio,omitempty"`

	// SidebarHidden persists the 'w' sidebar toggle across TUI launches.
	// False (default) = visible. Width-based auto-collapse is not recorded
	// here — only explicit toggles.
	SidebarHidden bool `json:"sidebar_hidden,omitempty"`

	// SidebarExpanded is the persisted per-row expand/collapse state for the
	// IDE-style sidebar tree. Keys follow the scheme used by sidebarNode.Key
	// (e.g. "wt:<LocalPath>", "older:wt", "older:s:<LocalPath>"). Absent or
	// false entries are collapsed. Older buckets reset to collapsed at every
	// launch regardless of what's stored here; the TUI is responsible for
	// that override.
	SidebarExpanded map[string]bool `json:"sidebar_expanded,omitempty"`

	// LastSessionByCwd records the session that was most recently open
	// when the TUI exited, keyed by the absolute cwd it was launched
	// from. On startup the TUI looks up the entry for the current cwd
	// and reopens that session — so "where I left off" survives across
	// runs without dropping users into a session from an unrelated
	// repo when they launch from elsewhere.
	LastSessionByCwd map[string]string `json:"last_session_by_cwd,omitempty"`

	// FlySprites configures the cloud-hub-side Fly.io Sprites launcher.
	// Only effective on a TCP-listening hub. Empty = launcher
	// disabled (sessions requesting launch_host.provider="flysprites"
	// will 4xx).
	FlySprites *FlySpritesPreference `json:"flysprites,omitempty"`

	// FlyMachines configures the raw Fly Machines provisioner. Nil =
	// disabled (sessions requesting launch_host.provider=
	// "flymachines" will 4xx).
	FlyMachines *FlyMachinesPreference `json:"flymachines,omitempty"`

	// DefaultLaunchHostProvider, when set, is applied to every new
	// session whose StartRequest omits LaunchHost. Use this on a
	// cloud hub to make TUI-created sessions automatically spin up
	// sandboxes (e.g. "flymachines") without each client having to know
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
	// The TUI auth panel and `clank remote` read the active remote via
	// Preferences.ActiveRemote().
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

// IsStaticBearer returns true for self-hosted profiles that authenticate
// with a fixed CLANK_AUTH_TOKEN rather than an OAuth session. Recognised
// by the presence of an access_token alongside the absence of every
// OAuth session field — no refresh token, no expiry, no JWT sub.
func (r *Remote) IsStaticBearer() bool {
	return r != nil && r.AccessToken != "" && r.RefreshToken == "" && r.ExpiresAt == 0 && r.UserID == ""
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
	p, _ := c.ActiveProfileAndName()
	return p
}

// ActiveProfileAndName returns the active Remote along with the key
// it lives under. Same fallback as ActiveProfile (when Active is
// empty or points at a missing profile, the alphabetically-first
// profile is selected). Callers that persist edits must use the
// returned name, not c.Active — the latter is the raw, possibly
// stale, on-disk value.
func (c *RemoteConfig) ActiveProfileAndName() (*Remote, string) {
	if c == nil || len(c.Profiles) == 0 {
		return nil, ""
	}
	if p, ok := c.Profiles[c.Active]; ok {
		return p, c.Active
	}
	name := firstRemoteName(c.Profiles)
	return c.Profiles[name], name
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

// ActiveRemoteAndName mirrors ActiveRemote but additionally returns
// the key the resolved profile lives under. Persistence-side callers
// (login, logout, refresh) must use this to avoid the (profile, "")
// mismatch when Active is empty/stale.
func (p *Preferences) ActiveRemoteAndName() (*Remote, string) {
	if p == nil || p.Remote == nil {
		return nil, ""
	}
	return p.Remote.ActiveProfileAndName()
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

// legacyModelMigrationBackend is the backend key the pre-split single
// "model" preference gets migrated into when DefaultBackend is empty.
// Mirrors agent.DefaultBackend's string form; this package can't import
// internal/agent (see the comment on Preferences.DefaultBackend).
const legacyModelMigrationBackend = "opencode"

// UnmarshalJSON on Preferences migrates two legacy shapes when the new
// keys are absent:
//   - top-level "cloud" → "remote"
//   - top-level "model" (single ModelPreference) → "models"[<backend>]
//
// The next SavePreferences emits only the new keys, quietly upgrading
// the file. Migration is non-destructive: a populated new key wins over
// the legacy value.
func (p *Preferences) UnmarshalJSON(data []byte) error {
	type alias Preferences
	aux := struct {
		*alias
		LegacyCloud *RemoteConfig    `json:"cloud,omitempty"`
		LegacyModel *ModelPreference `json:"model,omitempty"`
	}{alias: (*alias)(p)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if p.Remote == nil && aux.LegacyCloud != nil {
		p.Remote = aux.LegacyCloud
	}
	if len(p.Models) == 0 && aux.LegacyModel != nil && !aux.LegacyModel.IsZero() {
		key := p.DefaultBackend
		if key == "" {
			key = legacyModelMigrationBackend
		}
		p.Models = map[string]ModelPreference{key: *aux.LegacyModel}
	}
	return nil
}

// ModelFor returns the persisted model preference for the given
// backend, or the zero value when none is set.
func (p *Preferences) ModelFor(backend string) ModelPreference {
	if p == nil || p.Models == nil {
		return ModelPreference{}
	}
	return p.Models[backend]
}

// SetModelFor records the model preference for the given backend.
// A zero pref clears the entry instead of persisting empty strings.
func (p *Preferences) SetModelFor(backend string, pref ModelPreference) {
	if p == nil {
		return
	}
	if pref.IsZero() {
		delete(p.Models, backend)
		return
	}
	if p.Models == nil {
		p.Models = make(map[string]ModelPreference, 1)
	}
	p.Models[backend] = pref
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

// SetLastSessionForCwd records the most recently active session for a cwd,
// consumed by the TUI's "reopen where I left off" restore on startup.
// No-op for empty cwd (would conflate unrelated launch contexts).
func SetLastSessionForCwd(cwd, sessionID string) error {
	if cwd == "" {
		return nil
	}
	return UpdatePreferences(func(p *Preferences) {
		if p.LastSessionByCwd == nil {
			p.LastSessionByCwd = map[string]string{}
		}
		p.LastSessionByCwd[cwd] = sessionID
	})
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
