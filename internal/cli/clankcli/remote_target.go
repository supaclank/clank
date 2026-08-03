package clankcli

import "github.com/supaclank/clank/internal/config"

// resolveRemoteTarget picks the remote a `--remote`-aware command
// (login, logout) should operate on. Honors an explicit name when
// given; otherwise falls back to the active remote. Returns the
// resolved profile plus its name, or (nil, "") when nothing matches —
// callers translate that to a "no such remote" / "no active remote"
// error.
//
// The active-fallback path goes through ActiveRemoteAndName so the
// returned name always matches the resolved profile, even when
// Remote.Active is empty/stale and the fallback to the alphabetically-
// first profile fires. Returning Remote.Active raw would silently
// route persistence to a "" key.
func resolveRemoteTarget(prefs config.Preferences, name string) (*config.Remote, string) {
	if prefs.Remote == nil {
		return nil, ""
	}
	if name != "" {
		return prefs.Remote.Profiles[name], name
	}
	return prefs.ActiveRemoteAndName()
}
