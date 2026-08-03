package clankcli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/supaclank/clank/internal/config"
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
and the TUI auth panel all target it.

With no subcommand, prints the configured remotes — active marked with
` + "`*`" + `. Pass -v for URLs and signed-in identity.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
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

	if !verbose {
		for _, name := range names {
			fmt.Fprintln(out, remoteNameCell(name, name == active))
		}
		return nil
	}

	// Verbose: align the name + gateway columns, then style each cell.
	// Names/URLs are ASCII so byte length == display width; padding is
	// added after the styled cell so ANSI codes don't skew alignment.
	type row struct {
		name, gw, identity string
		active             bool
	}
	rows := make([]row, 0, len(names))
	nameW, gwW := 0, 0
	for _, name := range names {
		r := prefs.Remote.Profiles[name]
		// nil entry is only reachable via a hand-edited preferences.json;
		// keep the listing usable instead of dereferencing it.
		gw, identity := "(invalid profile)", "(not signed in)"
		if r != nil {
			gw = r.GatewayURL
			if gw == "" {
				gw = "(no gateway_url)"
			}
			switch {
			case r.UserEmail != "":
				identity = r.UserEmail
			case r.IsStaticBearer():
				identity = "(static bearer)"
			case r.AccessToken != "":
				identity = "(signed in)"
			}
		}
		rows = append(rows, row{name: name, gw: gw, identity: identity, active: name == active})
		nameW = max(nameW, len(name))
		gwW = max(gwW, len(gw))
	}
	for _, r := range rows {
		identity := styleDim.Render(r.identity)
		if r.identity == "(not signed in)" {
			identity = styleWarn.Render(r.identity)
		}
		fmt.Fprintf(out, "%s   %s   %s\n",
			remoteNameCell(r.name, r.active)+strings.Repeat(" ", nameW-len(r.name)),
			styleDim.Render(r.gw)+strings.Repeat(" ", gwW-len(r.gw)),
			identity,
		)
	}
	return nil
}

// remoteNameCell renders the active marker + remote name: a green "*" and
// a bold name for the active remote, a two-space indent otherwise.
func remoteNameCell(name string, active bool) string {
	if active {
		return styleOK.Render("*") + " " + styleWorktree.Render(name)
	}
	return "  " + name
}

func remoteSwitchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "switch <name>",
		Short: "Set the active remote",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
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
		token      string
	)
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Create a new remote (and set it active)",
		Long: `Add a named remote. The new remote becomes active so subsequent
push/pull calls target it. Repeating with the same name overwrites the
remote.

Token is the bearer the gateway requires. Normal flow is to leave
--token empty and run ` + "`clank login`" + ` to populate it — clank
fetches the OAuth endpoints from <gateway-url>/auth-config and runs
PKCE against them. Set --token directly only for self-hosted
static-bearer deployments (server-side CLANK_AUTH_TOKEN + CLANK_AUTH_ALLOW_STATIC=true).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
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
			cmd.SilenceUsage = true
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
