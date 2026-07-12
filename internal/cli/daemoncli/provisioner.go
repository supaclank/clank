package daemoncli

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/acksell/clank/internal/config"
	"github.com/acksell/clank/internal/store"
	"github.com/acksell/clank/pkg/provisioner"
	daytonaprov "github.com/acksell/clank/pkg/provisioner/daytona"
	flyioprov "github.com/acksell/clank/pkg/provisioner/flyio"
	flymachinesprov "github.com/acksell/clank/pkg/provisioner/flymachines"
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
	case "daytona":
		return buildDaytonaProvisioner(opts, st, prefs)
	case "flyio":
		return buildFlyIOProvisioner(opts, st, prefs)
	case "flymachines":
		return buildFlyMachinesProvisioner(opts, st, prefs)
	default:
		return nil, nil, fmt.Errorf("unknown provisioner %q (configure preferences.default_launch_host_provider to one of: local, daytona, flyio, flymachines)", provType)
	}
}

func buildLocalProvisioner() (provisioner.Provisioner, func(), error) {
	dir, err := config.Dir()
	if err != nil {
		return nil, nil, fmt.Errorf("config dir: %w", err)
	}
	prov := localprov.New(localprov.Options{
		// Each laptop daemon has its own host data dir alongside
		// the daemon's own clank.db. clank-host's --data-dir flag
		// receives this; it opens host.db inside.
		DataDir:            filepath.Join(dir, "host"),
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
	}, log.Default())
	return prov, prov.Stop, nil
}

func buildDaytonaProvisioner(opts ServerOptions, st *store.Store, prefs config.Preferences) (provisioner.Provisioner, func(), error) {
	if prefs.Daytona == nil || prefs.Daytona.APIKey == "" {
		return nil, nil, fmt.Errorf("daytona provisioner: preferences.daytona.api_key required")
	}
	prov, err := daytonaprov.New(daytonaprov.Options{
		APIKey:   prefs.Daytona.APIKey,
		Snapshot: prefs.Daytona.Snapshot,
		Image:    prefs.Daytona.Image,
		APIUrl:   prefs.Daytona.BaseURL,
		ExtraEnv: prefs.Daytona.ExtraEnv,
	}, st, log.Default())
	if err != nil {
		return nil, nil, fmt.Errorf("build daytona provisioner: %w", err)
	}
	return prov, prov.Stop, nil
}

func buildFlyMachinesProvisioner(opts ServerOptions, st *store.Store, prefs config.Preferences) (provisioner.Provisioner, func(), error) {
	fm := prefs.FlyMachines
	if fm == nil {
		return nil, nil, fmt.Errorf("flymachines provisioner: preferences.flymachines required")
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
		// Forward the builtin template catalog to the machine host
		// (host owns GET /templates), same env the sprite provider reads.
		TemplatesJSON: os.Getenv("CLANK_TEMPLATES"),
	}, st, log.Default())
	if err != nil {
		return nil, nil, fmt.Errorf("build flymachines provisioner: %w", err)
	}
	return prov, prov.Stop, nil
}

func buildFlyIOProvisioner(opts ServerOptions, st *store.Store, prefs config.Preferences) (provisioner.Provisioner, func(), error) {
	if prefs.FlyIO == nil || prefs.FlyIO.APIToken == "" {
		return nil, nil, fmt.Errorf("flyio provisioner: preferences.flyio.api_token required")
	}
	prov, err := flyioprov.New(flyioprov.Options{
		APIToken:            prefs.FlyIO.APIToken,
		OrganizationSlug:    prefs.FlyIO.OrganizationSlug,
		Region:              prefs.FlyIO.Region,
		SpriteNamePrefix:    prefs.FlyIO.SpriteNamePrefix,
		RamMB:               prefs.FlyIO.RamMB,
		CPUs:                prefs.FlyIO.CPUs,
		StorageGB:           prefs.FlyIO.StorageGB,
		NotifierWebhookURL:  notifierWebhookURL(),
		GitHubOAuthClientID: os.Getenv("CLANK_GITHUB_OAUTH_CLIENT_ID"),
	}, st, log.Default())
	if err != nil {
		return nil, nil, fmt.Errorf("build flyio provisioner: %w", err)
	}
	return prov, prov.Stop, nil
}
