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

// TestPreviewLinkCarriesNoSecret pins the PR-E contract: the QR is a
// tokenless invitation — a scanning phone authenticates through the
// typed-code ceremony, never a secret read off the code.
func TestPreviewLinkCarriesNoSecret(t *testing.T) {
	t.Parallel()
	encoded, err := PreviewLink{
		GatewayURL: "http://100.99.1.2:7880",
		PreviewURL: "http://192.168.1.20:8081",
		WorktreeID: "L1VzZXJzL21lL215LWV4cG8tYXBw",
	}.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if strings.Contains(encoded, "tok=") {
		t.Fatalf("preview link leaked a secret param: %q", encoded)
	}
}

func TestPreviewLinkEncodeRequiresGateway(t *testing.T) {
	t.Parallel()
	if _, err := (PreviewLink{PreviewURL: "http://192.168.1.20:8081"}).Encode(); err == nil {
		t.Fatal("expected error when GatewayURL is empty")
	}
}

func TestParsePreviewLinkRejectsForeignScheme(t *testing.T) {
	t.Parallel()
	cases := []string{
		"clank://preview?url=exp://x", // existing preview-only deep link, not a pairing link
		"https://link?gw=x&name=y",    // right path, wrong scheme
		"clank://link?name=y",         // missing gateway
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
