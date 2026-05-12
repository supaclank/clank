package clankcli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/acksell/clank/internal/config"
)

// remoteCmd registers `clank remote` — manage the named clank
// deployments (gateway + auth URLs + session) the user can target.
// Modeled on git remotes: one Active at a time, named entries,
// add/switch/remove subcommands.
//
// Bare `clank remote` lists names (active marked with `*`); `-v`
// includes URLs and signed-in identity. Matches `git remote` /
// `git remote -v` so it's immediately familiar.
//
// Remotes let the user keep several deployments wired up (dev docker
// stack, managed cloud, enterprise self-host) without rewriting
// preferences when they switch.
func remoteCmd() *cobra.Command {
	var verbose bool
	cmd := &cobra.Command{
		Use:   "remote",
		Short: "Manage clank-deployment remotes (like git remotes)",
		Long: `Manage named clank deployments in preferences.json.

A remote bundles a gateway URL, an auth-server URL, and the device-flow
session for one deployment. One remote is active at a time; push, pull,
migration, and the TUI auth panel all target it.

With no subcommand, prints the configured remotes — active marked with
` + "`*`" + `. Pass -v for URLs and signed-in identity.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRemoteList(cmd, verbose)
		},
	}
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "Include gateway URL and signed-in identity")
	cmd.AddCommand(
		remoteSwitchCmd(),
		remoteAddCmd(),
		remoteRemoveCmd(),
	)
	return cmd
}

// runRemoteList renders the configured remotes. Bare form is just the
// names (with `*` on active); verbose adds the gateway URL and
// signed-in email, mirroring `git remote -v`.
func runRemoteList(cmd *cobra.Command, verbose bool) error {
	prefs, err := config.LoadPreferences()
	if err != nil {
		return fmt.Errorf("load preferences: %w", err)
	}
	if prefs.Remote == nil || len(prefs.Remote.Profiles) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "no remotes configured. Use `clank remote add <name>` to create one.")
		return nil
	}
	names := make([]string, 0, len(prefs.Remote.Profiles))
	for k := range prefs.Remote.Profiles {
		names = append(names, k)
	}
	sort.Strings(names)
	active := prefs.Remote.Active
	out := cmd.OutOrStdout()
	for _, name := range names {
		marker := "  "
		if name == active {
			marker = "* "
		}
		if !verbose {
			fmt.Fprintf(out, "%s%s\n", marker, name)
			continue
		}
		r := prefs.Remote.Profiles[name]
		if r == nil {
			// nil entry only reachable via hand-edited preferences.json;
			// keep listing usable instead of panicking on the deref below.
			fmt.Fprintf(out, "%s%s\t(invalid profile)\t(not signed in)\n", marker, name)
			continue
		}
		gw := r.GatewayURL
		if gw == "" {
			gw = "(no gateway_url)"
		}
		identity := "(not signed in)"
		switch {
		case r.UserEmail != "":
			identity = r.UserEmail
		case r.AccessToken != "" && r.AuthURL == "":
			// Static-bearer profile (dev / self-hosted with no auth server).
			// No identity to show; signal it's a token-based remote.
			identity = "(static bearer)"
		}
		fmt.Fprintf(out, "%s%s\t%s\t%s\n", marker, name, gw, identity)
	}
	return nil
}

func remoteSwitchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "switch <name>",
		Short: "Set the active remote",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			if name == "" {
				return fmt.Errorf("remote name is required")
			}
			var found bool
			err := config.UpdatePreferences(func(p *config.Preferences) {
				if p.Remote == nil || p.Remote.Profiles == nil {
					return
				}
				if _, ok := p.Remote.Profiles[name]; !ok {
					return
				}
				p.Remote.Active = name
				found = true
			})
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("no remote named %q — run `clank remote` to see configured remotes", name)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "active remote: %s\n", name)
			return nil
		},
	}
}

func remoteAddCmd() *cobra.Command {
	var (
		gatewayURL string
		authURL    string
		token      string
	)
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Create a new remote (and set it active)",
		Long: `Add a named remote. The new remote becomes active so subsequent
push/pull calls target it. Repeating with the same name overwrites the
remote.

Token is the bearer the gateway requires. Normal flow is to leave
--token empty and run ` + "`clank login`" + ` to populate it via the device
flow against --auth-url. Set --token directly only for self-hosted
static-bearer deployments (server-side CLANK_AUTH_TOKEN + CLANK_AUTH_ALLOW_STATIC=true).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			if name == "" {
				return fmt.Errorf("remote name is required")
			}
			if gatewayURL == "" {
				return fmt.Errorf("--gateway-url is required")
			}
			err := config.UpdatePreferences(func(p *config.Preferences) {
				if p.Remote == nil {
					p.Remote = &config.RemoteConfig{Profiles: map[string]*config.Remote{}}
				}
				if p.Remote.Profiles == nil {
					p.Remote.Profiles = map[string]*config.Remote{}
				}
				p.Remote.Profiles[name] = &config.Remote{
					GatewayURL:  gatewayURL,
					AuthURL:     authURL,
					AccessToken: token,
				}
				p.Remote.Active = name
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "added remote %q → %s (active)\n", name, gatewayURL)
			return nil
		},
	}
	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "", "Gateway URL (required)")
	cmd.Flags().StringVar(&authURL, "auth-url", "", "Auth-server URL for device flow (optional)")
	// No backticks in the description — pflag treats backtick-quoted
	// substrings as the placeholder type name, which renders
	// "--token clank login" in --help and looks like the flag takes two
	// arguments.
	cmd.Flags().StringVar(&token, "token", "", "Bearer token for the gateway (optional; populated by 'clank login')")
	return cmd
}

func remoteRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "remove <name>",
		Aliases: []string{"rm"},
		Short:   "Delete a remote",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			var removed bool
			err := config.UpdatePreferences(func(p *config.Preferences) {
				if p.Remote == nil || p.Remote.Profiles == nil {
					return
				}
				if _, ok := p.Remote.Profiles[name]; !ok {
					return
				}
				removed = true
				delete(p.Remote.Profiles, name)
				if p.Remote.Active == name {
					p.Remote.Active = ""
					// Deterministic fallback: pick the lowest-name remote so
					// `remote remove` is reproducible across runs.
					keys := make([]string, 0, len(p.Remote.Profiles))
					for k := range p.Remote.Profiles {
						keys = append(keys, k)
					}
					sort.Strings(keys)
					if len(keys) > 0 {
						p.Remote.Active = keys[0]
					}
				}
			})
			if err != nil {
				return err
			}
			if !removed {
				return fmt.Errorf("no remote named %q", name)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed remote %q\n", name)
			return nil
		},
	}
}
