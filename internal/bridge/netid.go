package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Network identifies the LAN the laptop currently sits on, for keying
// per-network trust. The fingerprint hashes the default gateway's MAC
// + the local subnet — permission-free (SSID needs location perms on
// modern macOS) and the same signal class Windows uses for its
// private/public network memory. It's a consent key, not a security
// boundary: spoofing it only replays a consent the user already gave
// somewhere.
type Network struct {
	Fingerprint string `json:"fingerprint,omitempty"` // sha256(mac|subnet) hex; "" when undetectable
	Label       string `json:"label,omitempty"`       // human hint for prompts/status, e.g. "router aa:bb:… (192.168.1.0/24)"
}

const netidCmdTimeout = 2 * time.Second

// CurrentNetwork fingerprints the active default-route network.
// Best-effort: any failure yields Fingerprint "" (treated as
// untrusted everywhere).
// TODO(ai-review): no Windows branch — defaultGatewayIP/gatewayMAC only
// know darwin/linux, so Windows always falls back to Tailscale-only.
// https://github.com/supaclank/clank/pull/175#discussion_r3609121605
func CurrentNetwork(ctx context.Context) Network {
	ctx, cancel := context.WithTimeout(ctx, netidCmdTimeout)
	defer cancel()

	gwIP := defaultGatewayIP(ctx)
	if gwIP == "" {
		return Network{}
	}
	mac := gatewayMAC(ctx, gwIP)
	if mac == "" {
		return Network{}
	}
	subnet := subnetContaining(gwIP)
	sum := sha256.Sum256([]byte(mac + "|" + subnet))
	return Network{
		Fingerprint: hex.EncodeToString(sum[:]),
		Label:       fmt.Sprintf("router %s (%s)", mac, subnet),
	}
}

func defaultGatewayIP(ctx context.Context) string {
	if runtime.GOOS == "darwin" {
		out, err := exec.CommandContext(ctx, "route", "-n", "get", "default").Output()
		if err != nil {
			return ""
		}
		return parseDarwinRouteGateway(string(out))
	}
	out, err := exec.CommandContext(ctx, "ip", "route", "show", "default").Output()
	if err != nil {
		return ""
	}
	return parseLinuxRouteGateway(string(out))
}

func gatewayMAC(ctx context.Context, gwIP string) string {
	if runtime.GOOS == "darwin" {
		out, err := exec.CommandContext(ctx, "arp", "-n", gwIP).Output()
		if err != nil {
			return ""
		}
		return parseARPMAC(string(out))
	}
	out, err := exec.CommandContext(ctx, "ip", "neigh", "show", gwIP).Output()
	if err != nil {
		return ""
	}
	return parseIPNeighMAC(string(out))
}

// parseDarwinRouteGateway pulls the "gateway:" line from
// `route -n get default`.
func parseDarwinRouteGateway(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if ip, ok := strings.CutPrefix(line, "gateway: "); ok {
			return strings.TrimSpace(ip)
		}
	}
	return ""
}

// parseLinuxRouteGateway pulls the via-address from
// `ip route show default` ("default via 192.168.1.1 dev wlan0 …").
func parseLinuxRouteGateway(out string) string {
	fields := strings.Fields(out)
	for i, f := range fields {
		if f == "via" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

var macRE = regexp.MustCompile(`(?i)\b([0-9a-f]{1,2}(?::[0-9a-f]{1,2}){5})\b`)

// parseARPMAC pulls the "at aa:bb:…" MAC from darwin `arp -n <ip>`.
// Normalized lowercase, zero-padded octets (BSD arp prints "0:1f:…").
func parseARPMAC(out string) string {
	if strings.Contains(out, "no entry") {
		return ""
	}
	return normalizeMAC(macRE.FindString(out))
}

// parseIPNeighMAC pulls the lladdr MAC from `ip neigh show <ip>`.
func parseIPNeighMAC(out string) string {
	fields := strings.Fields(out)
	for i, f := range fields {
		if f == "lladdr" && i+1 < len(fields) {
			return normalizeMAC(fields[i+1])
		}
	}
	return ""
}

// normalizeMAC lowercases and zero-pads octets so the same router
// always fingerprints identically across arp output dialects.
func normalizeMAC(mac string) string {
	if mac == "" {
		return ""
	}
	parts := strings.Split(strings.ToLower(mac), ":")
	if len(parts) != 6 {
		return ""
	}
	for i, p := range parts {
		if len(p) == 1 {
			parts[i] = "0" + p
		}
	}
	return strings.Join(parts, ":")
}

// subnetContaining finds the local interface network holding ip,
// falling back to a /24 assumption when interfaces can't be read.
func subnetContaining(gwIP string) string {
	ip := net.ParseIP(gwIP)
	if ip == nil {
		return gwIP
	}
	ifaces, err := net.Interfaces()
	if err == nil {
		for _, iface := range ifaces {
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			for _, a := range addrs {
				ipnet, ok := a.(*net.IPNet)
				if ok && ipnet.Contains(ip) {
					return ipnet.String()
				}
			}
		}
	}
	if ip4 := ip.To4(); ip4 != nil {
		fallback := net.IPNet{IP: ip4.Mask(net.CIDRMask(24, 32)), Mask: net.CIDRMask(24, 32)}
		return fallback.String()
	}
	fallback := net.IPNet{IP: ip.Mask(net.CIDRMask(64, 128)), Mask: net.CIDRMask(64, 128)}
	return fallback.String()
}
