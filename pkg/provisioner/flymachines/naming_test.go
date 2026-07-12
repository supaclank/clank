package flymachines

import (
	"regexp"
	"strings"
	"testing"
)

func TestAppNameFor(t *testing.T) {
	t.Parallel()

	// Fly app names: lowercase alphanumerics + hyphens. The hash keeps
	// arbitrary userIDs (UUIDs, emails, symbols) valid and opaque.
	valid := regexp.MustCompile(`^[a-z0-9-]+$`)
	users := []string{
		"7ba73f13-5147-4f07-bb2f-9c1e2a3b4c5d",
		"user@example.com",
		"UPPER_case!!",
		"",
	}
	seen := map[string]string{}
	for _, u := range users {
		name := appNameFor("clank-u", u)
		if !valid.MatchString(name) {
			t.Errorf("appNameFor(%q) = %q: invalid app name", u, name)
		}
		if !strings.HasPrefix(name, "clank-u-") {
			t.Errorf("appNameFor(%q) = %q: missing prefix", u, name)
		}
		if len(name) > 30 {
			t.Errorf("appNameFor(%q) = %q: too long (%d)", u, name, len(name))
		}
		if prev, dup := seen[name]; dup {
			t.Errorf("collision: %q and %q both map to %q", prev, u, name)
		}
		seen[name] = u
		// Deterministic.
		if again := appNameFor("clank-u", u); again != name {
			t.Errorf("appNameFor(%q) not deterministic: %q vs %q", u, name, again)
		}
		// Must not leak the tenant identifier (names are visible via
		// cert-transparency-style surfaces).
		if u != "" && strings.Contains(name, strings.ToLower(u)) {
			t.Errorf("appNameFor(%q) = %q leaks the userID", u, name)
		}
	}
}

// TestAppNameForHashWidth pins the truncation at 8 bytes (64 bits):
// the app name is the tenant-isolation boundary, and ensureApp treats
// an existing app as "ours" without verifying ownership, so a narrower
// hash would make cross-tenant collisions a real risk at scale.
func TestAppNameForHashWidth(t *testing.T) {
	t.Parallel()
	name := appNameFor("clank-u", "user-1")
	hexPart := strings.TrimPrefix(name, "clank-u-")
	if len(hexPart) != 16 {
		t.Errorf("appNameFor hash suffix = %q (%d hex chars), want 16 (64 bits)", hexPart, len(hexPart))
	}
}

func TestDerivedNames(t *testing.T) {
	t.Parallel()
	app := appNameFor("clank-u", "user-1")

	if got := networkNameFor(app); got != app+"-net" {
		t.Errorf("networkNameFor = %q, want %q", got, app+"-net")
	}
	h := hostnameFor(app)
	if !strings.HasPrefix(h, "flym-") {
		t.Errorf("hostnameFor = %q, want flym- prefix", h)
	}
	if len(h) > len("flym-")+12 {
		t.Errorf("hostnameFor = %q: suffix exceeds 12 chars", h)
	}
}
