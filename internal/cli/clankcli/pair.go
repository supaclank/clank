package clankcli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	daemonclient "github.com/acksell/clank/internal/daemonclient"
)

// pairCmd is the phone↔laptop connection surface. The bare command
// shows the credential QR; subcommands administer the standing secret.
// CLI vocabulary is "pair" — the daemon-side subsystem is "bridge".
func pairCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pair",
		Short: "Connect your phone to this laptop",
		Long: "Shows a QR code that connects the clank app to this laptop's daemon.\n" +
			"Scan it once — the phone remembers the connection and finds the laptop\n" +
			"again on its own (instantly over Tailscale; on trusted Wi-Fi otherwise).",
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			return runPairQR()
		},
	}
	cmd.AddCommand(pairStatusCmd(), pairRevokeCmd())
	return cmd
}

func pairStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the bridge's addresses, network trust, and connection state",
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			return runPairStatus()
		},
	}
}

func pairRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke",
		Short: "Disconnect every phone by rotating the laptop's secret",
		Long: "Rotates the laptop's pairing secret. Every connected phone is\n" +
			"disconnected immediately; scan `clank pair` again to reconnect.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			return runPairRevoke()
		},
	}
}

func runPairQR() error {
	client, err := ensureDaemon()
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := ensurePhoneReachable(ctx, client, os.Stdin, os.Stdout)
	if err != nil {
		return err
	}

	hostname, _ := os.Hostname()
	link := PreviewLink{
		GatewayURL: st.URLs[0],
		Alts:       st.URLs[1:],
		Token:      st.PairToken,
		Name:       hostname,
	}
	linkStr, err := link.Encode()
	if err != nil {
		return err
	}

	fmt.Println("⚠  This code grants full access to this laptop — treat it like a password.")
	fmt.Println("   Scan it with the clank app (revoke every phone with `clank pair revoke`):")
	fmt.Println()
	printQR(linkStr)
	fmt.Printf("Reachable at: %s\n", strings.Join(st.URLs, ", "))
	if st.Tailnet == nil {
		fmt.Println("Tip: with Tailscale on both devices, the connection is encrypted and works from anywhere.")
	}
	return nil
}

func runPairStatus() error {
	client, err := ensureDaemon()
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := bridgeStatus(ctx, client)
	if err != nil {
		return err
	}

	if st.FirstConnected {
		fmt.Println("Paired: a phone has connected with the current secret.")
	} else {
		fmt.Println("Not paired yet: no phone has connected with the current secret — run `clank pair`.")
	}
	if len(st.URLs) > 0 {
		fmt.Printf("Reachable at: %s\n", strings.Join(st.URLs, ", "))
	} else {
		fmt.Println("Reachable at: nowhere yet (no tailnet, and this network isn't trusted).")
	}
	if st.Tailnet != nil {
		name := st.Tailnet.DNSName
		if name == "" {
			name = st.Tailnet.IP
		}
		fmt.Printf("Tailscale: active (%s)\n", name)
	} else {
		fmt.Println("Tailscale: not detected")
	}
	if st.Network.Fingerprint != "" {
		trust := "untrusted (plain-LAN serving off)"
		if st.NetworkTrusted {
			trust = "trusted (plain-LAN serving on)"
		}
		label := st.Network.Label
		if label == "" {
			label = "current network"
		}
		fmt.Printf("Network: %s — %s\n", label, trust)
	} else {
		fmt.Println("Network: unidentified — plain-LAN serving off")
	}
	for _, b := range st.Binds {
		if b.Err != "" {
			fmt.Printf("Bind error: %s:%d (%s): %s\n", b.IP, st.Port, b.Reason, b.Err)
		}
	}
	return nil
}

func runPairRevoke() error {
	client, err := ensureDaemon()
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	fmt.Print("Disconnect every phone paired with this laptop? [y/N] ")
	if !readYes(os.Stdin) {
		fmt.Println("Nothing revoked.")
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := client.Bridge().Rotate(ctx); err != nil {
		return fmt.Errorf("rotate secret: %w", err)
	}
	fmt.Println("All phones disconnected. Run `clank pair` to reconnect one.")
	return nil
}

// ensurePhoneReachable returns a bridge status with at least one
// phone-reachable URL, running the per-network trust prompt when a
// plain LAN is the only path and hasn't been consented to yet. The
// prompt is interactive-only; scripts get an actionable error.
func ensurePhoneReachable(ctx context.Context, client *daemonclient.Client, in io.Reader, out io.Writer) (*daemonclient.BridgeStatus, error) {
	st, err := bridgeStatus(ctx, client)
	if err != nil {
		return nil, err
	}
	if len(st.URLs) > 0 {
		return st, nil
	}
	if st.LANIP != "" && !st.NetworkTrusted {
		if !stdinIsTTY(in) {
			return nil, fmt.Errorf("this network isn't trusted for unencrypted serving — run `clank pair` interactively to approve it, or connect both devices to Tailscale")
		}
		if st.Network.Fingerprint == "" {
			return nil, fmt.Errorf("couldn't identify this network to remember a trust choice — connect Tailscale for encrypted access instead")
		}
		label := st.Network.Label
		if label == "" {
			label = "this network"
		}
		fmt.Fprintf(out, "No Tailscale connection — serving to your phone over %s would be UNENCRYPTED.\n", label)
		fmt.Fprint(out, "Trust this network? [y/N] ")
		if !readYes(in) {
			return nil, fmt.Errorf("not serving on an untrusted network — connect both devices to Tailscale for encrypted access, or re-run and answer y")
		}
		// Fresh timeout for the daemon call: readYes above waits on the
		// user, which must not eat into the RPC's own deadline.
		trustCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		st, err = client.Bridge().TrustNetwork(trustCtx, st.Network.Fingerprint, st.Network.Label)
		if err != nil {
			return nil, fmt.Errorf("trust network: %w", err)
		}
		if len(st.URLs) > 0 {
			return st, nil
		}
	}
	return nil, fmt.Errorf("your phone can't reach this laptop yet — connect to Wi-Fi or Tailscale and retry")
}

// bridgeStatus fetches bridge state, translating the 404 an old
// (pre-bridge) daemon returns into an actionable hint.
func bridgeStatus(ctx context.Context, client *daemonclient.Client) (*daemonclient.BridgeStatus, error) {
	st, err := client.Bridge().Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("bridge status (running daemon may predate `clank pair` — restart it): %w", err)
	}
	return st, nil
}

// readYes consumes one line and reports an explicit yes.
func readYes(in io.Reader) bool {
	line, _ := bufio.NewReader(in).ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

// stdinIsTTY reports whether in is an interactive terminal — the
// trust prompt never blocks a script. os.ModeCharDevice alone would
// misidentify /dev/null (a common non-interactive stand-in) as a TTY.
func stdinIsTTY(in io.Reader) bool {
	f, ok := in.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}
