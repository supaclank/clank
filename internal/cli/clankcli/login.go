package clankcli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/supaclank/clank/internal/cloud"
	"github.com/supaclank/clank/internal/config"
	"github.com/supaclank/clank/internal/daemonclient"
)

// loginCmd registers `clank login` — drive the OAuth 2.0 authorization
// code + PKCE dance against the active remote's gateway, then persist
// the resulting access/refresh tokens on that remote's entry. Same
// code path the TUI's cloud panel uses; this is the terminal-only
// entry point.
//
// Discovery: the gateway exposes /auth-config returning standard
// OAuth 2.0 endpoints (authorize, token, client_id, scopes). clank
// runs PKCE against them; the browser handles the actual sign-in
// (GitHub / Google / SSO / etc. as configured in the IdP dashboard).
//
// Targets the active remote by default; --remote selects a different
// one without flipping which is active.
func loginCmd() *cobra.Command {
	var (
		remoteName string
		provider   string
	)
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in to a remote via OAuth (PKCE in your browser)",
		Long: `Authenticate against the gateway_url of a configured remote and
store the access token on that remote's entry in preferences.json.

clank opens your browser to the IdP (discovered via
<gateway_url>/auth-config), and you sign in there. The browser
redirects to a localhost listener clank spawns; the token round-trips
back into the prefs file.

Defaults to the active remote; pass --remote to log in to a different
remote (without changing which is active). The remote must have
gateway_url set; configure it via ` + "`clank remote add <name> --gateway-url=...`" + `.

Doesn't work over SSH or in containers (localhost callback can't reach
the user's browser). Workaround: ssh -L <port>:localhost:<port>.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			return runLogin(cmd.Context(), cmd, remoteName, provider)
		},
	}
	cmd.Flags().StringVar(&remoteName, "remote", "", "Remote name to log in to (default: active remote)")
	cmd.Flags().StringVar(&provider, "provider", "", "OAuth provider override (default: server's default_provider, if any)")
	return cmd
}

// runLogin drives the OAuth PKCE sign-in against a remote's gateway and
// persists the session. remoteName "" targets the active remote.
func runLogin(ctx context.Context, cmd *cobra.Command, remoteName, provider string) error {
	prefs, err := config.LoadPreferences()
	if err != nil {
		return fmt.Errorf("load preferences: %w", err)
	}
	target, name := resolveRemoteTarget(prefs, remoteName)
	if target == nil {
		if remoteName == "" {
			return fmt.Errorf("no active remote configured; run `clank remote add <name> --gateway-url=...` first")
		}
		return fmt.Errorf("no remote named %q", remoteName)
	}
	if target.GatewayURL == "" {
		return fmt.Errorf("remote %q has no gateway_url; add one with `clank remote add %s --gateway-url=...`", name, name)
	}

	loginCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	// Discover IdP via /auth-config.
	gw := cloud.New(target.GatewayURL, nil)
	fmt.Fprintf(cmd.OutOrStdout(), "Discovering auth config at %s … ", target.GatewayURL)
	cfg, err := gw.FetchAuthConfig(loginCtx)
	if err != nil {
		return fmt.Errorf("\nfetch auth-config: %w", err)
	}
	fmt.Fprintln(cmd.OutOrStdout(), "ok")

	provName := provider
	if provName == "" {
		provName = cfg.DefaultProvider
	}

	if provName != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "Opening browser for sign-in via %s … ", provName)
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "Opening browser for sign-in … ")
	}
	oauth := &cloud.OAuthClient{
		AuthorizeEndpoint: cfg.AuthorizeEndpoint,
		TokenEndpoint:     cfg.TokenEndpoint,
		ClientID:          cfg.ClientID,
		Scopes:            cfg.Scopes,
		Provider:          provName,
		CallbackPort:      cfg.CallbackPort,
		Prompt:            cmd.ErrOrStderr(),
	}
	session, err := oauth.Login(loginCtx)
	if err != nil {
		return fmt.Errorf("\nsign-in: %w", err)
	}
	fmt.Fprintln(cmd.OutOrStdout(), "ok")

	if err := daemonclient.WriteRemoteSession(name, session); err != nil {
		return fmt.Errorf("save session: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "\n%s Signed in to remote %q as %s\n", styleOK.Render("✓"), name, session.UserEmail)
	return nil
}
