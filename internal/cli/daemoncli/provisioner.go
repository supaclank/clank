package daemoncli

import (
	"fmt"
	"log"
	"path/filepath"

	"github.com/acksell/clank/internal/config"
	"github.com/acksell/clank/pkg/provisioner"
	daytonaprov "github.com/acksell/clank/pkg/provisioner/daytona"
	flyioprov "github.com/acksell/clank/pkg/provisioner/flyio"
	localprov "github.com/acksell/clank/pkg/provisioner/local"
	"github.com/acksell/clank/internal/store"
)

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
	default:
		return nil, nil, fmt.Errorf("unknown provisioner %q (configure preferences.default_launch_host_provider to one of: local, daytona, flyio)", provType)
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
		DataDir: filepath.Join(dir, "host"),
	}, log.Default())
	return prov, prov.Stop, nil
}

func buildDaytonaProvisioner(opts ServerOptions, st *store.Store, prefs config.Preferences) (provisioner.Provisioner, func(), error) {
	if prefs.Daytona == nil || prefs.Daytona.APIKey == "" {
		return nil, nil, fmt.Errorf("daytona provisioner: preferences.daytona.api_key required")
	}
	if prefs.RemoteHub == nil || prefs.RemoteHub.AuthToken == "" {
		return nil, nil, fmt.Errorf("daytona provisioner: preferences.remote_hub.auth_token required")
	}
	if opts.PublicBaseURL == "" {
		return nil, nil, fmt.Errorf("daytona provisioner: --public-base-url required so sandboxes can reach the hub")
	}
	prov, err := daytonaprov.New(daytonaprov.Options{
		APIKey:       prefs.Daytona.APIKey,
		Snapshot:     prefs.Daytona.Snapshot,
		Image:        prefs.Daytona.Image,
		APIUrl:       prefs.Daytona.BaseURL,
		ExtraEnv:     prefs.Daytona.ExtraEnv,
		HubBaseURL:   opts.PublicBaseURL,
		HubAuthToken: prefs.RemoteHub.AuthToken,
	}, st, log.Default())
	if err != nil {
		return nil, nil, fmt.Errorf("build daytona provisioner: %w", err)
	}
	return prov, prov.Stop, nil
}

func buildFlyIOProvisioner(opts ServerOptions, st *store.Store, prefs config.Preferences) (provisioner.Provisioner, func(), error) {
	if prefs.FlyIO == nil || prefs.FlyIO.APIToken == "" {
		return nil, nil, fmt.Errorf("flyio provisioner: preferences.flyio.api_token required")
	}
	if prefs.RemoteHub == nil || prefs.RemoteHub.AuthToken == "" {
		return nil, nil, fmt.Errorf("flyio provisioner: preferences.remote_hub.auth_token required")
	}
	if opts.PublicBaseURL == "" {
		return nil, nil, fmt.Errorf("flyio provisioner: --public-base-url required")
	}
	prov, err := flyioprov.New(flyioprov.Options{
		APIToken:         prefs.FlyIO.APIToken,
		OrganizationSlug: prefs.FlyIO.OrganizationSlug,
		Region:           prefs.FlyIO.Region,
		SpriteNamePrefix: prefs.FlyIO.SpriteNamePrefix,
		RamMB:            prefs.FlyIO.RamMB,
		CPUs:             prefs.FlyIO.CPUs,
		StorageGB:        prefs.FlyIO.StorageGB,
		HubBaseURL:       opts.PublicBaseURL,
		HubAuthToken:     prefs.RemoteHub.AuthToken,
	}, st, log.Default())
	if err != nil {
		return nil, nil, fmt.Errorf("build flyio provisioner: %w", err)
	}
	return prov, prov.Stop, nil
}
