// Package lannet answers "what address can peers on the local network
// reach this host at" — shared by the CLI (QR links) and the daemon's
// bridge listener.
package lannet

import (
	"fmt"
	"net"
)

// LANIP returns a non-loopback IPv4 address other devices on the local
// network can reach this host at — the address the QR advertises so the
// phone can connect to the laptop's gateway and Metro.
//
// Primary method is the UDP-dial trick: "connecting" a UDP socket to a
// public address makes the kernel select the egress interface (no packets
// are sent), and its local address is the one a peer on that path sees.
// Falls back to scanning interfaces for a private (RFC1918) IPv4 when the
// dial can't resolve a usable address (e.g. no default route).
//
// NB: with a VPN/tailscale exit node active, the dial trick returns the
// tunnel address (e.g. 100.64/10) — callers that need a physical-LAN
// address should check IsCGNAT.
func LANIP() (net.IP, error) {
	if ip := dialLANIP(); ip != nil {
		return ip, nil
	}
	return scanLANIP()
}

// IsCGNAT reports whether ip is in 100.64.0.0/10 — the shared-address
// range Tailscale assigns tailnet devices from. Used to tell a tailnet
// address apart from a physical LAN one.
func IsCGNAT(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	return ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127
}

func dialLANIP() net.IP {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return nil
	}
	defer conn.Close()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return nil
	}
	ip4 := addr.IP.To4()
	if ip4 == nil || ip4.IsLoopback() {
		return nil
	}
	return ip4
}

func scanLANIP() (net.IP, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("enumerate interfaces: %w", err)
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			ip4 := ip.To4()
			if ip4 == nil || ip4.IsLoopback() || !ip4.IsPrivate() {
				continue
			}
			return ip4, nil
		}
	}
	return nil, fmt.Errorf("no private LAN IPv4 found — connect to a network, or use --tunnel for off-LAN access")
}

// TailnetIP scans interfaces for a CGNAT (100.64/10) IPv4 — present
// exactly when Tailscale is up, regardless of install flavor (GUI
// network extension or tailscaled daemon).
func TailnetIP() net.IP {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if IsCGNAT(ipnet.IP) {
				return ipnet.IP.To4()
			}
		}
	}
	return nil
}
