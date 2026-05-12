package clankcli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/acksell/clank/internal/cloud"
	"github.com/acksell/clank/internal/config"
)

// loginCmd registers `clank login` — drive an RFC 8628 device flow
// against the active remote's auth server, then persist the resulting
// access/refresh tokens on that remote's entry. Same code path the
// TUI's cloud panel uses; this is the terminal-only entry point.
//
// Targets the active remote by default; --remote selects a different
// one without flipping which is active.
func loginCmd() *cobra.Command {
	var remoteName string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in to a remote via OAuth device flow",
		Long: `Authenticate against the auth_url of a configured remote and store
the access token on that remote's entry in preferences.json.

Defaults to the active remote; pass --remote to log in to a different
remote (without changing which is active). The remote must have
auth_url set; configure it via ` + "`clank remote add <name> --auth-url=...`" + `.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			prefs, err := config.LoadPreferences()
			if err != nil {
				return fmt.Errorf("load preferences: %w", err)
			}
			target, name := resolveLoginTarget(prefs, remoteName)
			if target == nil {
				if remoteName == "" {
					return fmt.Errorf("no active remote configured; run `clank remote add <name> --gateway-url=... --auth-url=...` first")
				}
				return fmt.Errorf("no remote named %q", remoteName)
			}
			if target.AuthURL == "" {
				return fmt.Errorf("remote %q has no auth_url; add one with `clank remote add %s --auth-url=...`", name, name)
			}

			c := cloud.New(target.AuthURL, nil)
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
			defer cancel()

			start, err := c.StartDeviceFlow(ctx)
			if err != nil {
				return fmt.Errorf("start device flow: %w", err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "\nTo sign in, visit:\n\n    %s\n\nand enter the code: %s\n\nWaiting for confirmation",
				preferVerificationURI(start), start.UserCode)

			session, err := pollDeviceFlow(ctx, cmd, c, start)
			if err != nil {
				return err
			}

			if err := persistRemoteSession(name, session); err != nil {
				return fmt.Errorf("save session: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), " ok\n\nSigned in to remote %q as %s\n", name, session.UserEmail)
			return nil
		},
	}
	cmd.Flags().StringVar(&remoteName, "remote", "", "Remote name to log in to (default: active remote)")
	return cmd
}

// resolveLoginTarget picks the remote to log in to and returns it plus
// the name. Honors --remote when given; falls back to active.
func resolveLoginTarget(prefs config.Preferences, name string) (*config.Remote, string) {
	if prefs.Remote == nil {
		return nil, ""
	}
	if name != "" {
		return prefs.Remote.Profiles[name], name
	}
	return prefs.ActiveRemote(), prefs.Remote.Active
}

// preferVerificationURI uses the *Complete variant when available
// (clickable link with code prefilled) and falls back to the bare URI.
func preferVerificationURI(s *cloud.DeviceStartResponse) string {
	if s.VerificationURIComplete != "" {
		return s.VerificationURIComplete
	}
	return s.VerificationURI
}

// pollDeviceFlow runs the standard RFC 8628 poll loop with a slow-down
// nudge on the matching error. Returns the session on success or
// surfaces any non-pending error to the caller.
func pollDeviceFlow(ctx context.Context, cmd *cobra.Command, c *cloud.Client, start *cloud.DeviceStartResponse) (*cloud.Session, error) {
	interval := time.Duration(start.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(time.Duration(start.ExpiresIn) * time.Second)
	if start.ExpiresIn <= 0 {
		deadline = time.Now().Add(15 * time.Minute)
	}
	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("\ndevice flow timed out before approval — re-run `clank login`")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
		session, err := c.PollDeviceFlow(ctx, start.DeviceCode)
		switch {
		case err == nil:
			return session, nil
		case errors.Is(err, cloud.ErrAuthorizationPending):
			fmt.Fprint(cmd.OutOrStdout(), ".")
			continue
		case errors.Is(err, cloud.ErrSlowDown):
			interval += 5 * time.Second
			continue
		case errors.Is(err, cloud.ErrAccessDenied):
			return nil, fmt.Errorf("\nsign-in was denied")
		case errors.Is(err, cloud.ErrExpiredToken):
			return nil, fmt.Errorf("\ndevice code expired before approval — re-run `clank login`")
		default:
			return nil, fmt.Errorf("\npoll: %w", err)
		}
	}
}

// persistRemoteSession writes the device-flow grant onto the named
// remote in preferences.json. Creates the remote entry if missing —
// possible when --remote names an unknown one (caller already
// validated, but UpdatePreferences runs against the latest disk
// version which a concurrent edit could have changed).
func persistRemoteSession(name string, s *cloud.Session) error {
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
		r.RefreshToken = s.RefreshToken
		r.UserEmail = s.UserEmail
		r.UserID = s.UserID
		r.ExpiresAt = s.ExpiresAt
	})
}
