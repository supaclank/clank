package clankcli

import (
	"fmt"
	"net"
)

// lanIP returns a non-loopback IPv4 address other devices on the local
// network can reach this host at — the address the QR advertises so the
// phone can connect to the laptop's gateway and Metro.
//
// Primary method is the UDP-dial trick: "connecting" a UDP socket to a
// public address makes the kernel select the egress interface (no packets
// are sent), and its local address is the one a peer on that path sees.
// Falls back to scanning interfaces for a private (RFC1918) IPv4 when the
// dial can't resolve a usable address (e.g. no default route).
func lanIP() (net.IP, error) {
	if ip := dialLANIP(); ip != nil {
		return ip, nil
	}
	return scanLANIP()
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
