package tokens

import (
	"strings"
	"testing"
)

func TestNew_UniqueAndDNSSafe(t *testing.T) {
	t.Parallel()
	seen := make(map[string]struct{})
	for range 1000 {
		tok, err := New()
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, dup := seen[tok]; dup {
			t.Fatalf("token collision after %d draws: %q", len(seen), tok)
		}
		seen[tok] = struct{}{}
		if len(tok) != 26 {
			t.Errorf("token length = %d, want 26 for 16 bytes base32-nopad: %q", len(tok), tok)
		}
		for _, r := range tok {
			isLower := r >= 'a' && r <= 'z'
			isDigit := r >= '2' && r <= '7'
			if !isLower && !isDigit {
				t.Errorf("token %q has non-DNS-safe rune %q", tok, r)
			}
		}
	}
}

func TestParseHost(t *testing.T) {
	t.Parallel()
	const root = "clankexample.dev"
	tests := []struct {
		name      string
		host      string
		wantToken string
		wantOK    bool
	}{
		{"happy", "preview-abc123.clankexample.dev", "abc123", true},
		{"with port", "preview-abc123.clankexample.dev:443", "abc123", true},
		{"wrong root", "preview-abc123.elsewhere.dev", "", false},
		{"no prefix", "api.clankexample.dev", "", false},
		{"empty token", "preview-.clankexample.dev", "", false},
		{"dot in token (would escape wildcard zone)", "preview-evil.subdomain.clankexample.dev", "", false},
		{"bare root", "clankexample.dev", "", false},
		{"prefix-only", "preview.clankexample.dev", "", false}, // missing the trailing dash → wrong leftmost label
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tok, ok := ParseHost(tc.host, root)
			if ok != tc.wantOK || tok != tc.wantToken {
				t.Errorf("ParseHost(%q) = (%q, %t), want (%q, %t)", tc.host, tok, ok, tc.wantToken, tc.wantOK)
			}
		})
	}
}

func TestHostFor_RoundTrip(t *testing.T) {
	t.Parallel()
	const root = "clankexample.dev"
	tok, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	host := HostFor(tok, root)
	parsed, ok := ParseHost(host, root)
	if !ok || parsed != tok {
		t.Fatalf("round-trip: HostFor(%q)=%q, ParseHost→(%q,%t)", tok, host, parsed, ok)
	}
	if !strings.HasPrefix(host, HostPrefix) {
		t.Errorf("HostFor missing %q prefix: %q", HostPrefix, host)
	}
}

func TestURLFor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		token  string
		root   string
		scheme string
		port   string
		want   string
	}{
		{"cloud default", "abc123", "clankexample.dev", "https", "", "https://preview-abc123.clankexample.dev/"},
		{"empty-scheme defaults to https", "abc123", "clankexample.dev", "", "", "https://preview-abc123.clankexample.dev/"},
		{"local docker", "abc123", "localhost", "http", "7878", "http://preview-abc123.localhost:7878/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := URLFor(tc.token, tc.root, tc.scheme, tc.port); got != tc.want {
				t.Errorf("URLFor = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestVisibility_Valid(t *testing.T) {
	t.Parallel()
	for _, ok := range []Visibility{VisibilityOwnerOnly, VisibilityPublic} {
		if !ok.Valid() {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []Visibility{"", "anyone", "OWNER_ONLY", "Public"} {
		if Visibility(bad).Valid() {
			t.Errorf("%q should be invalid", bad)
		}
	}
}
