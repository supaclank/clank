package daemoncli

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/acksell/clank/internal/config"
	"github.com/acksell/clank/internal/store"
	"github.com/acksell/clank/pkg/provisioner"
	flymachinesprov "github.com/acksell/clank/pkg/provisioner/flymachines"
	flyspritesprov "github.com/acksell/clank/pkg/provisioner/flysprites"
	localprov "github.com/acksell/clank/pkg/provisioner/local"
)

// notifierWebhookURL returns the URL clank-host should POST notification
// webhooks to. Sourced from CLANK_NOTIFIER_WEBHOOK_URL; empty when
// unset, which disables the notifier subsystem inside the provisioned
// host. Operators set this to their clankd / gateway's public URL,
// suffixed with /webhooks/notifications.
func notifierWebhookURL() string {
	return os.Getenv("CLANK_NOTIFIER_WEBHOOK_URL")
}

// previewWebhookURL returns the URL clank-host should POST preview
// register/revoke webhooks to. Sourced from CLANK_PREVIEW_WEBHOOK_URL.
// Empty disables the preview-route registration — the dev server
// still spawns but no public token is minted (the local/docker-dev
// path can leave this empty if not testing the gateway integration).
func previewWebhookURL() string {
	return os.Getenv("CLANK_PREVIEW_WEBHOOK_URL")
}

// Default builtin create-project template: the project's curated Expo
// starter, so a zero-config daemon can scaffold a phone-previewable
// Expo app out of the box. Deliberate exception to the no-vendor-refs
// rule — the starter's contents are brand-neutral.
const (
	defaultTemplateDisplayName = "Expo app"
	defaultTemplateCloneURL    = "https://github.com/supaclank/expo-56-starter-template.git"
)

// defaultTemplates returns the builtin catalog clankd serves when the
// operator hasn't configured CLANK_TEMPLATES.
func defaultTemplates() []provisioner.Template {
	return []provisioner.Template{{
		DisplayName: defaultTemplateDisplayName,
		CloneURL:    defaultTemplateCloneURL,
	}}
}

// builtinTemplates resolves the builtin create-project catalog every
// provisioner forwards to clank-host. This is the env→config edge: the
// library takes []provisioner.Template, and clankd (this binary) is the
// thing that turns an env var into it.
//
// CLANK_TEMPLATES (a JSON array of {display_name, clone_url}) replaces
// the default catalog when set; an explicit empty array ("[]") disables
// builtin templates — the host then serves only the user's own GitHub
// template repos. Unset (or empty, which env passthroughs like
// docker-compose produce for unset host vars) → defaultTemplates().
func builtinTemplates() ([]provisioner.Template, error) {
	raw := os.Getenv("CLANK_TEMPLATES")
	if raw == "" {
		return defaultTemplates(), nil
	}
	var templates []provisioner.Template
	if err := json.Unmarshal([]byte(raw), &templates); err != nil {
		return nil, fmt.Errorf("CLANK_TEMPLATES: invalid JSON: %w", err)
	}
	// JSON "null" unmarshals into a nil slice without error; reject it
	// rather than silently disabling builtin templates.
	if templates == nil {
		return nil, fmt.Errorf("CLANK_TEMPLATES: must be a JSON array, got %q", raw)
	}
	return templates, nil
}

// buildProvisioner picks the active provisioner for the gateway based
// on preferences.default_launch_host_provider. Defaults to local
// (subprocess) when unset — the laptop-mode default.
//
// cleanup is non-nil when the chosen provisioner owns goroutines or
// subprocess children that need explicit Stop on shutdown.
func buildProvisioner(opts ServerOptions, st *store.Store) (provisioner.Provisioner, func(), error) {
	if st == nil {
		return nil, nil, fmt.Errorf("provisioner: store is required")
	}
	prefs, err := config.LoadPreferences()
	if err != nil {
		return nil, nil, fmt.Errorf("load preferences: %w", err)
	}

	provType := "local"
	if prefs.DefaultLaunchHostProvider != "" {
		provType = prefs.DefaultLaunchHostProvider
	}

	switch provType {
	case "local", "local-stub":
		return buildLocalProvisioner()
	case "flysprites":
		return buildFlySpritesProvisioner(opts, st, prefs)
	case "flymachines":
		return buildFlyMachinesProvisioner(opts, st, prefs)
	default:
		return nil, nil, fmt.Errorf("unknown provisioner %q (configure preferences.default_launch_host_provider to one of: local, flysprites, flymachines)", provType)
	}
}

func buildLocalProvisioner() (provisioner.Provisioner, func(), error) {
	dir, err := config.Dir()
	if err != nil {
		return nil, nil, fmt.Errorf("config dir: %w", err)
	}
	templates, err := builtinTemplates()
	if err != nil {
		return nil, nil, err
	}
	prov := localprov.New(localprov.Options{
		// Each laptop daemon has its own host data dir alongside
		// the daemon's own clank.db. clank-host's --data-dir flag
		// receives this; it opens host.db inside.
		DataDir: filepath.Join(dir, "host"),
		// Worktrees + repo canonicals live under the clank config dir
		// (~/.clank/work), not the sprite-style $HOME/work — the laptop
		// host shares the user's home and must not litter it.
		WorkRoot:           filepath.Join(dir, "work"),
		NotifierWebhookURL: notifierWebhookURL(),
		PreviewWebhookURL:  previewWebhookURL(),
		// CLANK_LOCAL_USER_ID must match the sub claim of JWTs the
		// gateway's authenticator accepts — otherwise the preview
		// surface's owner-only check 404s legitimate requests as
		// cross-tenant.
		UserID: os.Getenv("CLANK_LOCAL_USER_ID"),
		// Opt-in: route preview conns through clank-host's /tunnel
		// endpoint (the machine-backend data path) instead of a
		// direct loopback dial — lets the docker dev stack exercise
		// the production preview path without a cloud provider.
		TunnelInternalConn: os.Getenv("CLANK_LOCAL_TUNNEL_INTERNAL_CONN") == "true",
		Templates:          templates,
	}, log.Default())
	return prov, prov.Stop, nil
}

func buildFlyMachinesProvisioner(opts ServerOptions, st *store.Store, prefs config.Preferences) (provisioner.Provisioner, func(), error) {
	fm := prefs.FlyMachines
	if fm == nil {
		return nil, nil, fmt.Errorf("flymachines provisioner: preferences.flymachines required")
	}
	templates, err := builtinTemplates()
	if err != nil {
		return nil, nil, err
	}
	prov, err := flymachinesprov.New(context.Background(), flymachinesprov.Options{
		APIToken:            fm.APIToken,
		OrgSlug:             fm.OrgSlug,
		Region:              fm.Region,
		Image:               fm.Image,
		GatewayNetwork:      fm.GatewayNetwork,
		AppNamePrefix:       fm.AppNamePrefix,
		GuestCPUKind:        fm.GuestCPUKind,
		GuestCPUs:           fm.GuestCPUs,
		GuestMemoryMB:       fm.GuestMemoryMB,
		SwapSizeMB:          fm.SwapSizeMB,
		VolumeSizeGB:        fm.VolumeSizeGB,
		NotifierWebhookURL:  notifierWebhookURL(),
		PreviewWebhookURL:   previewWebhookURL(),
		GitHubOAuthClientID: os.Getenv("CLANK_GITHUB_OAUTH_CLIENT_ID"),
		Templates:           templates,
	}, st, log.Default())
	if err != nil {
		return nil, nil, fmt.Errorf("build flymachines provisioner: %w", err)
	}
	return prov, prov.Stop, nil
}

func buildFlySpritesProvisioner(opts ServerOptions, st *store.Store, prefs config.Preferences) (provisioner.Provisioner, func(), error) {
	if prefs.FlySprites == nil || prefs.FlySprites.APIToken == "" {
		return nil, nil, fmt.Errorf("flysprites provisioner: preferences.flysprites.api_token required")
	}
	templates, err := builtinTemplates()
	if err != nil {
		return nil, nil, err
	}
	prov, err := flyspritesprov.New(flyspritesprov.Options{
		APIToken:            prefs.FlySprites.APIToken,
		OrganizationSlug:    prefs.FlySprites.OrganizationSlug,
		Region:              prefs.FlySprites.Region,
		SpriteNamePrefix:    prefs.FlySprites.SpriteNamePrefix,
		RamMB:               prefs.FlySprites.RamMB,
		CPUs:                prefs.FlySprites.CPUs,
		StorageGB:           prefs.FlySprites.StorageGB,
		NotifierWebhookURL:  notifierWebhookURL(),
		GitHubOAuthClientID: os.Getenv("CLANK_GITHUB_OAUTH_CLIENT_ID"),
		Templates:           templates,
	}, st, log.Default())
	if err != nil {
		return nil, nil, fmt.Errorf("build flysprites provisioner: %w", err)
	}
	return prov, prov.Stop, nil
}
