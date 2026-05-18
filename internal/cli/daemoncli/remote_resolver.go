package daemoncli

import (
	"log"

	"github.com/acksell/clank/internal/config"
)

// prefsRemoteResolver satisfies gateway.RemoteResolver by reading the
// laptop's preferences.json each time. Stateless beyond a log handle —
// the file is re-read every call so an in-flight `clank remote switch`
// is picked up on the next OwnerCache refresh without a daemon restart.
type prefsRemoteResolver struct {
	log *log.Logger
}

func newPrefsRemoteResolver(lg *log.Logger) *prefsRemoteResolver {
	if lg == nil {
		lg = log.Default()
	}
	return &prefsRemoteResolver{log: lg}
}

// ActiveRemote returns the active remote's gateway URL and bearer
// token, or ok=false when no active remote is configured. Treats a
// missing gateway_url as not-configured rather than panicking — the
// gateway's session router falls back to local routing in that case.
func (r *prefsRemoteResolver) ActiveRemote() (string, string, bool) {
	prefs, err := config.LoadPreferences()
	if err != nil {
		r.log.Printf("remote_resolver: load preferences: %v", err)
		return "", "", false
	}
	p := prefs.ActiveRemote()
	if p == nil {
		return "", "", false
	}
	if p.GatewayURL == "" {
		return "", "", false
	}
	return p.GatewayURL, p.AccessToken, true
}
