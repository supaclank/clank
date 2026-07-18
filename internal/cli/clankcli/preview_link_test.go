package clankcli

import (
	"github.com/acksell/clank/internal/lannet"
	"net"
	"reflect"
	"strings"
	"testing"
)

func TestPreviewLinkRoundTrip(t *testing.T) {
	t.Parallel()
	want := PreviewLink{
		GatewayURL: "http://100.99.1.2:7880",
		Alts:       []string{"http://axels-mbp.tail1234.ts.net:7880", "http://192.168.1.20:7880"},
		Token:      "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE",
		PreviewURL: "http://192.168.1.20:8081",
		SessionID:  "01HSESSION",
		LocalPath:  "/Users/me/my-expo-app",
		Backend:    "claude-code",
		Name:       "my expo app",
		WorktreeID: "L1VzZXJzL21lL215LWV4cG8tYXBw",
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
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

// TestPreviewLinkTokenlessRoundTrip pins the invitation shape: once a
// phone has connected, preview QRs carry no secret — just where to
// reach the bridge.
func TestPreviewLinkTokenlessRoundTrip(t *testing.T) {
	t.Parallel()
	want := PreviewLink{
		GatewayURL: "http://100.99.1.2:7880",
		PreviewURL: "http://192.168.1.20:8081",
		LocalPath:  "/Users/me/my-expo-app",
		Backend:    "claude-code",
		WorktreeID: "L1VzZXJzL21lL215LWV4cG8tYXBw",
	}
	encoded, err := want.Encode()
	if err != nil {
		t.Fatalf("Encode tokenless: %v", err)
	}
	if strings.Contains(encoded, "tok=") {
		t.Fatalf("tokenless link leaked a tok param: %q", encoded)
	}
	got, err := ParsePreviewLink(encoded)
	if err != nil {
		t.Fatalf("ParsePreviewLink: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestPreviewLinkEncodeRequiresGateway(t *testing.T) {
	t.Parallel()
	if _, err := (PreviewLink{Token: "t"}).Encode(); err == nil {
		t.Fatal("expected error when GatewayURL is empty")
	}
}

func TestParsePreviewLinkRejectsForeignScheme(t *testing.T) {
	t.Parallel()
	cases := []string{
		"clank://preview?url=exp://x", // existing preview-only deep link, not a pairing link
		"https://link?gw=x&tok=y",     // right path, wrong scheme
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
	ip, err := lannet.LANIP()
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
