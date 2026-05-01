package daemoncli

import (
	"fmt"

	"github.com/acksell/clank/internal/config"
	daytonalauncher "github.com/acksell/clank/internal/host/launcher/daytona"
)

// buildDaytonaLauncher loads preferences and constructs a Daytona
// launcher when configured. Returns (nil, nil) when preferences don't
// have a daytona block — that's the "not configured" signal, not an
// error.
//
// PublicBaseURL is required: sandboxes need a way to reach the cloud
// hub for git mirror clones. We refuse to wire up the launcher
// without it rather than spawning sandboxes that can't function.
func buildDaytonaLauncher(opts ServerOptions) (*daytonalauncher.Launcher, error) {
	prefs, err := config.LoadPreferences()
	if err != nil {
		return nil, fmt.Errorf("load preferences: %w", err)
	}
	if prefs.Daytona == nil || prefs.Daytona.APIKey == "" {
		return nil, nil
	}
	if prefs.RemoteHub == nil || prefs.RemoteHub.AuthToken == "" {
		return nil, fmt.Errorf("daytona requires preferences.remote_hub.auth_token")
	}
	if opts.PublicBaseURL == "" {
		return nil, fmt.Errorf("daytona requires --public-base-url so sandboxes can reach the hub")
	}
	return daytonalauncher.New(daytonalauncher.Options{
		APIKey:       prefs.Daytona.APIKey,
		Snapshot:     prefs.Daytona.Snapshot,
		Image:        prefs.Daytona.Image,
		APIUrl:       prefs.Daytona.BaseURL, // pref name kept for back-compat; SDK calls it APIUrl
		ExtraEnv:     prefs.Daytona.ExtraEnv,
		HubBaseURL:   opts.PublicBaseURL,
		HubAuthToken: prefs.RemoteHub.AuthToken,
	}, nil)
}
