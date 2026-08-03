package daemonclient

// refresh.go — keeps the active remote's OAuth access_token fresh.
//
// OAuth access tokens expire (commonly 1h on Supabase, Auth0, etc.).
// Without proactive refresh, `clank status`/`push`/`pull` would 401
// despite the user having a perfectly valid refresh_token sitting in
// preferences.json. EnsureFreshActiveRemote closes that gap: callers
// invoke it before any authenticated gateway call; it discovers the
// IdP via /auth-config, runs the refresh_token grant, and persists
// the rotated tokens.

import (
	"context"
	"fmt"
	"time"

	"github.com/supaclank/clank/internal/cloud"
	"github.com/supaclank/clank/internal/config"
)

// refreshGracePeriod controls how early a token gets pre-emptively
// refreshed. Refreshing only when already expired races the actual
// gateway call; the grace window guarantees the in-flight request
// has a non-stale bearer.
const refreshGracePeriod = 60 * time.Second

// EnsureFreshActiveRemote refreshes the active remote's OAuth tokens
// when the access_token is within refreshGracePeriod of expiry and a
// refresh_token is available. No-op for static-bearer profiles, for
// profiles without an expires_at, or when the token is still fresh.
//
// Returns cloud.ErrUnauthorized (wrapped) when the IdP rejects the
// refresh — callers should surface "run `clank login`". Returns other
// errors for transient failures (network, gateway 5xx); callers decide
// whether to abort or proceed with the stale token.
func EnsureFreshActiveRemote(ctx context.Context) error {
	prefs, err := config.LoadPreferences()
	if err != nil {
		return fmt.Errorf("load preferences: %w", err)
	}
	// Resolve profile and name together so a successful refresh
	// persists under the same key the profile actually lives under.
	// Going through Remote.Active raw would write to "" when Active
	// is empty/stale and the fallback to the alphabetically-first
	// profile fires — creating a phantom profile and leaving the real
	// one stuck on stale tokens.
	p, name := prefs.ActiveRemoteAndName()
	if p == nil || p.IsStaticBearer() {
		return nil
	}
	if p.RefreshToken == "" || p.ExpiresAt == 0 {
		return nil
	}
	if time.Now().Unix() < p.ExpiresAt-int64(refreshGracePeriod.Seconds()) {
		return nil
	}

	gw := cloud.New(p.GatewayURL, nil)
	cfg, err := gw.FetchAuthConfig(ctx)
	if err != nil {
		return fmt.Errorf("discover auth-config for refresh: %w", err)
	}
	oc := &cloud.OAuthClient{
		TokenEndpoint: cfg.TokenEndpoint,
		ClientID:      cfg.ClientID,
	}
	session, err := oc.Refresh(ctx, p.RefreshToken)
	if err != nil {
		// Wraps cloud.ErrUnauthorized when the IdP returns 401; callers
		// detect it with errors.Is and route to "run `clank login`".
		return fmt.Errorf("refresh access token: %w", err)
	}

	return WriteRemoteSession(name, session)
}

// WriteRemoteSession persists an OAuth grant onto the named remote in
// preferences.json. Creates the remote entry if it's somehow missing
// (UpdatePreferences runs against the latest disk version which a
// concurrent edit could have changed since the caller loaded prefs).
//
// Used by `clank login` after a fresh sign-in and by
// EnsureFreshActiveRemote after a successful refresh. Some IdPs rotate
// refresh tokens; the existing token is preserved when the response
// omits one (the spec allows that, RFC 6749 §6).
func WriteRemoteSession(name string, s *cloud.Session) error {
	return config.UpdatePreferences(func(p *config.Preferences) {
		if p.Remote == nil {
			p.Remote = &config.RemoteConfig{Profiles: map[string]*config.Remote{}}
		}
		if p.Remote.Profiles == nil {
			p.Remote.Profiles = map[string]*config.Remote{}
		}
		r, ok := p.Remote.Profiles[name]
		if !ok {
			r = &config.Remote{}
			p.Remote.Profiles[name] = r
		}
		r.AccessToken = s.AccessToken
		if s.RefreshToken != "" {
			r.RefreshToken = s.RefreshToken
		}
		if s.UserEmail != "" {
			r.UserEmail = s.UserEmail
		}
		if s.UserID != "" {
			r.UserID = s.UserID
		}
		r.ExpiresAt = s.ExpiresAt
	})
}
