package flyio

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/acksell/clank/pkg/provisioner"
	"github.com/acksell/clank/internal/store"
)

func mustOpenStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestNew_FailsFastOnMissingOptions pins the construction guards.
func TestNew_FailsFastOnMissingOptions(t *testing.T) {
	t.Parallel()
	s := mustOpenStore(t)
	cases := []struct {
		name string
		opts Options
	}{
		{"missing-api-token", Options{}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if _, err := New(c.opts, s, nil); err == nil {
				t.Errorf("New(%+v) returned nil error", c.opts)
			}
		})
	}
}

// TestNew_RejectsNilStore enforces the precondition that a real store
// is wired in. Without it, persistence is silently broken.
func TestNew_RejectsNilStore(t *testing.T) {
	t.Parallel()
	if _, err := New(Options{APIToken: "tok"}, nil, nil); err == nil {
		t.Error("New with nil store returned nil error")
	}
}

// TestSafeSpriteSuffix locks the userID-to-sprite-suffix sanitizer.
// Sprites accept lowercase alphanum + hyphen; everything else is
// stripped silently rather than failing — the userID source (PR 1
// hardcodes "local"; PR 4 uses real user IDs) doesn't always control
// what characters appear.
func TestSafeSpriteSuffix(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"local":         "local",
		"User_42":       "user42",
		"Alice@Bob.com": "alicebobcom",
		"abc-123":       "abc-123",
		"   spaces   ":  "spaces",
		"":              "",
	}
	for in, want := range cases {
		if got := safeSpriteSuffix(in); got != want {
			t.Errorf("safeSpriteSuffix(%q) = %q; want %q", in, got, want)
		}
	}
}

// TestSafeHostnameSuffix locks the trailing-segment cap mirroring the
// Daytona convention. The host catalog assumes Hostname uniqueness
// across providers AND a stable shape; drift here would produce
// "host not registered" errors that are hard to debug.
func TestSafeHostnameSuffix(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"clank-host-local":          "local",
		"clank-host-abcdefghijklmn": "abcdefghijkl",
		"plain":                     "plain",
	}
	for in, want := range cases {
		if got := safeHostnameSuffix(in); got != want {
			t.Errorf("safeHostnameSuffix(%q) = %q; want %q", in, got, want)
		}
	}
}

// Bearer-injector unit tests live alongside the shared type in
// internal/provisioner/transport — the flyio provisioner just wires
// it together with the parsed sprite URL host.

// TestSpriteNameFor_DistinctInputsDistinctNames pins the regression
// CR raised: pre-fix safeSpriteSuffix collapsed distinct user IDs to
// the same suffix (e.g. "A.B" and "ab" → "ab"), so user A would
// target user B's persistent sprite. The fix appends a short hash of
// the raw userID whenever the sanitization is lossy.
func TestSpriteNameFor_DistinctInputsDistinctNames(t *testing.T) {
	t.Parallel()
	p := &Provisioner{opts: Options{SpriteNamePrefix: "clank-host"}}
	cases := []string{
		"alice@example.com",
		"alice.example.com",
		"aliceexamplecom", // already-clean, would collide pre-fix
		"A.B",
		"ab",
		"!!!", // symbol-only
		"???", // distinct symbol-only
	}
	seen := map[string]string{}
	for _, in := range cases {
		got := p.spriteNameFor(in)
		if other, dup := seen[got]; dup {
			t.Errorf("collision: spriteNameFor(%q) and spriteNameFor(%q) both yield %q", other, in, got)
		}
		seen[got] = in
	}

	// Sanity: a clean input round-trips without a hash suffix.
	if got := p.spriteNameFor("local"); got != "clank-host-local" {
		t.Errorf("clean input: got %q, want %q (hash suffix should only fire on lossy inputs)", got, "clank-host-local")
	}
}

// TestEmbeddedBinary_NotEmpty pins that the //go:embed actually
// produced bytes — a missing or zero-size binary would be an
// invisible build-pipeline regression that surfaces only at runtime
// when a sprite tries to install a 0-byte clank-host.
func TestEmbeddedBinary_NotEmpty(t *testing.T) {
	t.Parallel()
	// 1MB is a generous floor — the cross-compiled clank-host is
	// ~17MB; anything under 1MB strongly implies a stub/empty file
	// was embedded by mistake.
	if len(clankHostBinary) < 1<<20 {
		t.Errorf("embedded clank-host binary suspiciously small: %d bytes (want > 1MB; rerun `make embed-host`)", len(clankHostBinary))
	}
}

// TestSpriteNameFor_UsesPrefixAndSanitizedSuffix exercises the
// composition of sprite names as the user/userID flow lands.
func TestSpriteNameFor_UsesPrefixAndSanitizedSuffix(t *testing.T) {
	t.Parallel()
	p := &Provisioner{opts: Options{SpriteNamePrefix: "clank-host"}}
	if got := p.spriteNameFor("local"); got != "clank-host-local" {
		t.Errorf("local: got %q", got)
	}
	// Empty userID falls through to "anonymous" placeholder so a bug
	// upstream surfaces as a deterministic name rather than a panic.
	if got := p.spriteNameFor(""); !strings.HasPrefix(got, "clank-host-") {
		t.Errorf("empty userID: got %q (want prefix clank-host-)", got)
	}
}

// _ asserts at compile time that *Provisioner satisfies the
// provisioner.Provisioner interface.
var _ provisioner.Provisioner = (*Provisioner)(nil)
