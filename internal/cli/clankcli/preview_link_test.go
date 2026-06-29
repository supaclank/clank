package clankcli

import (
	"net"
	"strings"
	"testing"
)

func TestPreviewLinkRoundTrip(t *testing.T) {
	t.Parallel()
	want := PreviewLink{
		GatewayURL: "http://192.168.1.20:7878",
		Token:      "pair_abc123",
		PreviewURL: "http://192.168.1.20:8081",
		SessionID:  "01HSESSION",
		LocalPath:  "/Users/me/my-expo-app",
		Backend:    "claude-code",
		Name:       "my expo app",
	}
	encoded, err := want.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !strings.HasPrefix(encoded, "clank://link?") {
		t.Fatalf("encoded link has unexpected prefix: %q", encoded)
	}
	got, err := ParsePreviewLink(encoded)
	if err != nil {
		t.Fatalf("ParsePreviewLink(%q): %v", encoded, err)
	}
	if got != want {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestPreviewLinkEncodeRequiresGatewayAndToken(t *testing.T) {
	t.Parallel()
	if _, err := (PreviewLink{Token: "t"}).Encode(); err == nil {
		t.Fatal("expected error when GatewayURL is empty")
	}
	if _, err := (PreviewLink{GatewayURL: "http://x"}).Encode(); err == nil {
		t.Fatal("expected error when Token is empty")
	}
}

func TestParsePreviewLinkRejectsForeignScheme(t *testing.T) {
	t.Parallel()
	cases := []string{
		"clank://preview?url=exp://x", // existing preview-only deep link, not a pairing link
		"https://link?gw=x&tok=y",     // right path, wrong scheme
		"clank://link?gw=x",           // missing token
		"clank://link?tok=y",          // missing gateway
	}
	for _, in := range cases {
		if _, err := ParsePreviewLink(in); err == nil {
			t.Errorf("ParsePreviewLink(%q): expected error, got nil", in)
		}
	}
}

func TestLANIPIsRoutableWhenFound(t *testing.T) {
	t.Parallel()
	ip, err := lanIP()
	if err != nil {
		t.Skipf("no LAN IP in this environment: %v", err) // CI without a routable interface
	}
	if ip.To4() == nil {
		t.Fatalf("lanIP returned non-IPv4: %v", ip)
	}
	if ip.IsLoopback() {
		t.Fatalf("lanIP returned loopback: %v", ip)
	}
	if ip.Equal(net.IPv4zero) {
		t.Fatalf("lanIP returned unspecified address")
	}
}
