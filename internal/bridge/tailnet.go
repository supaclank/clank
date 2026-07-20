package bridge

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"time"

	"github.com/acksell/clank/internal/lannet"
)

// Tailnet describes the laptop's Tailscale presence, when any.
type Tailnet struct {
	IP      string `json:"ip"`                 // 100.64/10 address, always set when active
	DNSName string `json:"dns_name,omitempty"` // MagicDNS name, best-effort (needs the CLI)
}

// tailscaleCLIPaths are tried in order for `tailscale status --json`.
// The macOS GUI install (network extension — no tailscaled process)
// ships its CLI inside the app bundle; the Homebrew/open-source
// flavor puts it on PATH.
var tailscaleCLIPaths = []string{
	"tailscale",
	"/Applications/Tailscale.app/Contents/MacOS/Tailscale",
}

const tailscaleCLITimeout = 2 * time.Second

// DiscoverTailnet reports the laptop's tailnet address, nil when
// Tailscale isn't up. The interface scan (100.64/10) is the primary,
// install-flavor-agnostic signal; the CLI only enriches with the
// MagicDNS name and is best-effort.
func DiscoverTailnet(ctx context.Context) *Tailnet {
	ip := lannet.TailnetIP()
	if ip == nil {
		return nil
	}
	t := &Tailnet{IP: ip.String()}
	if name := magicDNSName(ctx); name != "" {
		t.DNSName = name
	}
	return t
}

// magicDNSName asks the local Tailscale for this device's DNS name.
// Empty on any failure — the 100.x IP alone is fully usable.
func magicDNSName(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, tailscaleCLITimeout)
	defer cancel()
	for _, bin := range tailscaleCLIPaths {
		out, err := exec.CommandContext(ctx, bin, "status", "--json").Output()
		if err != nil {
			continue
		}
		return parseMagicDNSName(out)
	}
	return ""
}

// parseMagicDNSName extracts Self.DNSName from `tailscale status
// --json`, trimming the trailing dot Tailscale appends.
func parseMagicDNSName(statusJSON []byte) string {
	var status struct {
		Self struct {
			DNSName string `json:"DNSName"`
		} `json:"Self"`
	}
	if err := json.Unmarshal(statusJSON, &status); err != nil {
		return ""
	}
	return strings.TrimSuffix(status.Self.DNSName, ".")
}
