package bridge

import "testing"

func TestParseDarwinRouteGateway(t *testing.T) {
	t.Parallel()
	out := `   route to: default
destination: default
       mask: default
    gateway: 192.168.10.1
  interface: en0
      flags: <UP,GATEWAY,DONE,STATIC,PRCLONING,GLOBAL>
`
	if got := parseDarwinRouteGateway(out); got != "192.168.10.1" {
		t.Errorf("gateway = %q, want 192.168.10.1", got)
	}
	if got := parseDarwinRouteGateway("route: no route to host\n"); got != "" {
		t.Errorf("no-route output must yield empty, got %q", got)
	}
}

func TestParseLinuxRouteGateway(t *testing.T) {
	t.Parallel()
	if got := parseLinuxRouteGateway("default via 10.0.0.1 dev wlan0 proto dhcp metric 600\n"); got != "10.0.0.1" {
		t.Errorf("gateway = %q, want 10.0.0.1", got)
	}
	if got := parseLinuxRouteGateway(""); got != "" {
		t.Errorf("empty output must yield empty, got %q", got)
	}
}

func TestParseARPMAC(t *testing.T) {
	t.Parallel()
	// BSD arp prints unpadded octets — normalization must zero-pad so
	// the same router always fingerprints identically.
	out := "? (192.168.10.1) at 0:1f:33:ab:cd:9 on en0 ifscope [ethernet]\n"
	if got := parseARPMAC(out); got != "00:1f:33:ab:cd:09" {
		t.Errorf("mac = %q, want 00:1f:33:ab:cd:09", got)
	}
	if got := parseARPMAC("? (192.168.10.7) -- no entry\n"); got != "" {
		t.Errorf("no-entry output must yield empty, got %q", got)
	}
}

func TestParseIPNeighMAC(t *testing.T) {
	t.Parallel()
	out := "10.0.0.1 dev wlan0 lladdr AA:BB:CC:DD:EE:FF REACHABLE\n"
	if got := parseIPNeighMAC(out); got != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("mac = %q, want aa:bb:cc:dd:ee:ff", got)
	}
	if got := parseIPNeighMAC("10.0.0.1 dev wlan0 FAILED\n"); got != "" {
		t.Errorf("failed neigh must yield empty, got %q", got)
	}
}

func TestParseMagicDNSName(t *testing.T) {
	t.Parallel()
	status := `{"Self":{"DNSName":"axels-mbp.tail1234.ts.net.","TailscaleIPs":["100.123.16.31"]}}`
	if got := parseMagicDNSName([]byte(status)); got != "axels-mbp.tail1234.ts.net" {
		t.Errorf("dns name = %q, want axels-mbp.tail1234.ts.net", got)
	}
	if got := parseMagicDNSName([]byte("not json")); got != "" {
		t.Errorf("bad json must yield empty, got %q", got)
	}
}
