package clankcli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	daemonclient "github.com/supaclank/clank/internal/daemonclient"
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
	cmd.AddCommand(pairStatusCmd(), pairListCmd(), pairRevokeCmd())
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

func pairListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the phones approved to connect to this laptop",
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			return runPairList()
		},
	}
}

func pairRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke [device]",
		Short: "Disconnect a paired phone (or every phone)",
		Long: "Removes a phone's key from this laptop's approved devices. With no\n" +
			"argument, removes every phone. Revoked phones re-pair by scanning\n" +
			"`clank pair` and typing the code again.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			device := ""
			if len(args) == 1 {
				device = args[0]
			}
			return runPairRevoke(device)
		},
	}
}

func runPairQR() error {
	client, err := ensureDaemon()
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(sigCtx, 30*time.Second)
	defer cancel()

	st, err := ensurePhoneReachable(ctx, client, os.Stdin, os.Stdout)
	if err != nil {
		return err
	}

	// The QR is all public: addresses + the laptop's identity key. A
	// new phone scans, shows a code, and you approve it below; a phone
	// that already paired reconnects on its own.
	link := PreviewLink{
		GatewayURL: st.URLs[0],
		Alts:       st.URLs[1:],
		HostKey:    st.HostKey,
		Name:       shortHostname(),
	}
	linkStr, err := link.Encode()
	if err != nil {
		return err
	}

	shownAt := time.Now()
	fmt.Println("Scan this with the clank app to connect your phone to this laptop:")
	fmt.Println()
	printQR(linkStr)
	fmt.Printf("Reachable at: %s\n", strings.Join(st.URLs, ", "))
	if st.Tailnet == nil {
		fmt.Println("Tip: with Tailscale on both devices, the connection is encrypted and works from anywhere.")
	}
	fmt.Println("\nWaiting for a scan… (Ctrl+C to stop)")

	// Service inbound pairing ceremonies (a new phone shows a code you
	// type here) concurrently with waiting for any phone — new or
	// returning — to authenticate.
	go pairingLoop(sigCtx, client, os.Stdin, os.Stdout)
	device, connected := waitForConnection(sigCtx, client, shownAt)
	if !connected {
		clearTerminal()
		fmt.Println("Stopped waiting. Run `clank pair` to show the code again.")
		return nil
	}
	clearTerminal()
	fmt.Printf("✓ %s connected — this laptop is now in your phone's gateway picker.\n", device)
	fmt.Println("  Manage with `clank pair status`; disconnect every phone with `clank pair revoke`.")
	return nil
}

// waitForConnection polls bridge status until a phone authenticates
// after shownAt, or ctx cancels. Returns the device name on success.
func waitForConnection(ctx context.Context, client *daemonclient.Client, shownAt time.Time) (device string, connected bool) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", false
		case <-ticker.C:
		}
		tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		st, err := client.Bridge().Status(tctx)
		cancel()
		if err != nil {
			continue
		}
		if st.LastConnectedAt != nil && st.LastConnectedAt.After(shownAt) {
			name := st.LastDevice
			if name == "" {
				name = "Your phone"
			}
			return name, true
		}
	}
}

// shortHostname is the laptop's display name for phones: the hostname
// with the domain suffix dropped ("Axels-MBP.lan" → "Axels-MBP") —
// full mDNS/DHCP suffixes read as noise in the picker.
func shortHostname() string {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		return "laptop"
	}
	return stripHostSuffix(hostname)
}

func stripHostSuffix(hostname string) string {
	if short, _, found := strings.Cut(hostname, "."); found && short != "" {
		return short
	}
	return hostname
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

	switch n := len(st.Devices); n {
	case 0:
		fmt.Println("Paired devices: none — run `clank pair` and approve your phone.")
	case 1:
		fmt.Printf("Paired devices: 1 (%s)\n", st.Devices[0].Name)
	default:
		fmt.Printf("Paired devices: %d — see `clank pair list`\n", n)
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

func runPairList() error {
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
	if len(st.Devices) == 0 {
		fmt.Println("No paired devices — run `clank pair` and approve your phone.")
		return nil
	}
	for _, d := range st.Devices {
		seen := "never seen"
		if d.LastSeen != nil {
			seen = "last seen " + d.LastSeen.Local().Format("Jan 2 15:04")
		}
		fmt.Printf("%s — paired %s, %s\n", d.Name, d.AddedAt.Local().Format("Jan 2"), seen)
	}
	return nil
}

func runPairRevoke(device string) error {
	client, err := ensureDaemon()
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if device == "" {
		fmt.Print("Disconnect every phone paired with this laptop? [y/N] ")
		if !readYes(os.Stdin) {
			fmt.Println("Nothing revoked.")
			return nil
		}
		if _, err := client.Bridge().RevokeAllDevices(ctx); err != nil {
			return fmt.Errorf("revoke devices: %w", err)
		}
		fmt.Println("All phones disconnected. Run `clank pair` to reconnect one.")
		return nil
	}

	st, err := bridgeStatus(ctx, client)
	if err != nil {
		return err
	}
	var matches []daemonclient.BridgeDevice
	for _, d := range st.Devices {
		if strings.EqualFold(d.Name, device) {
			matches = append(matches, d)
		}
	}
	switch len(matches) {
	case 0:
		return fmt.Errorf("no paired device named %q — see `clank pair list`", device)
	case 1:
		if _, err := client.Bridge().RevokeDevice(ctx, matches[0].PubKey); err != nil {
			return fmt.Errorf("revoke device: %w", err)
		}
		fmt.Printf("%s disconnected. It can re-pair via `clank pair`.\n", matches[0].Name)
		return nil
	default:
		return fmt.Errorf("%d paired devices are named %q — run `clank pair revoke` (no argument) to disconnect all, then re-pair the ones you keep", len(matches), device)
	}
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
		// TODO(ai-review): context.Background() means Ctrl+C during this
		// call can't cancel it — needs the signal-only ctx threaded in.
		// https://github.com/supaclank/clank/pull/175#discussion_r3609121580
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

// bridgeStatus fetches bridge state, wrapping any transport error with
// a hint that the daemon may predate `clank pair` and need a restart.
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
// trust prompt never blocks a script.
func stdinIsTTY(in io.Reader) bool { return fileIsTTY(in) }
