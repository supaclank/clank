package clankcli

import "github.com/acksell/clank/internal/config"

// resolveRemoteTarget picks the remote a `--remote`-aware command
// (login, logout) should operate on. Honors an explicit name when
// given; otherwise falls back to the active remote. Returns the
// resolved profile plus its name, or (nil, "") when nothing matches —
// callers translate that to a "no such remote" / "no active remote"
// error.
func resolveRemoteTarget(prefs config.Preferences, name string) (*config.Remote, string) {
	if prefs.Remote == nil {
		return nil, ""
	}
	if name != "" {
		return prefs.Remote.Profiles[name], name
	}
	return prefs.ActiveRemote(), prefs.Remote.Active
}
