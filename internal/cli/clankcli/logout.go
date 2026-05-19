package clankcli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/acksell/clank/internal/config"
)

// logoutCmd registers `clank logout` — clear the OAuth session for a
// remote without dropping its gateway URL. Mirrors `clank login`'s
// shape: defaults to the active remote, accepts --remote to target a
// specific one.
//
// Keeps the remote entry itself; only the session fields (access /
// refresh tokens, user email/id, expiry) get cleared. Re-running
// `clank login` after logout works without re-adding the remote.
//
// No server-side token revocation. Most IdPs don't expose RFC 7009;
// the access token expires on its own (usually within an hour) and
// the refresh token becomes unreachable once cleared locally.
func logoutCmd() *cobra.Command {
	var remoteName string
	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Clear the OAuth session for a remote (keeps gateway_url)",
		Long: `Clear the OAuth session stored for a remote in preferences.json.

By default, logs out of the active remote; pass --remote to target a
specific one. The remote's gateway_url is preserved so a subsequent
` + "`clank login`" + ` works without re-adding the remote.

Idempotent: running logout on a remote that's already signed out
prints a notice and exits 0.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			prefs, err := config.LoadPreferences()
			if err != nil {
				return fmt.Errorf("load preferences: %w", err)
			}
			target, name := resolveRemoteTarget(prefs, remoteName)
			if target == nil {
				if remoteName == "" {
					return fmt.Errorf("no active remote configured")
				}
				return fmt.Errorf("no remote named %q", remoteName)
			}
			if target.AccessToken == "" && target.RefreshToken == "" && target.UserEmail == "" && target.UserID == "" && target.ExpiresAt == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "not signed in to remote %q\n", name)
				return nil
			}
			err = config.UpdatePreferences(func(p *config.Preferences) {
				if p.Remote == nil || p.Remote.Profiles == nil {
					return
				}
				r := p.Remote.Profiles[name]
				if r == nil {
					return
				}
				r.AccessToken = ""
				r.RefreshToken = ""
				r.UserEmail = ""
				r.UserID = ""
				r.ExpiresAt = 0
			})
			if err != nil {
				return fmt.Errorf("clear session: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Signed out of remote %q\n", name)
			return nil
		},
	}
	cmd.Flags().StringVar(&remoteName, "remote", "", "Remote name to log out of (default: active remote)")
	return cmd
}
