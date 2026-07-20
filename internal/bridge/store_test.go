package bridge

import (
	"bytes"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"
)

func testStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bridge.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	// Tests assert persistence by reopening — debounce off.
	s.touchFlushInterval = 0
	return s, path
}

func devKey(t *testing.T, fill byte) (ed25519.PrivateKey, []byte) {
	t.Helper()
	seed := bytes.Repeat([]byte{fill}, KeyLen)
	priv := ed25519.NewKeyFromSeed(seed)
	return priv, priv.Public().(ed25519.PublicKey)
}

func TestOpenStoreMintsHostKeyAndPersists(t *testing.T) {
	t.Parallel()
	s, path := testStore(t)

	pub := s.HostPublicKey()
	if len(pub) != KeyLen {
		t.Fatalf("host pubkey length = %d, want %d", len(pub), KeyLen)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("bridge.json mode = %o, want 600", perm)
	}

	// Reopen loads the same identity — the host key survives restarts.
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if !bytes.Equal(s2.HostPublicKey(), pub) {
		t.Fatalf("reopened host key differs from minted key")
	}

	// And it actually signs: a probe answer verifies against the pubkey.
	nonce := bytes.Repeat([]byte{0xAB}, probeNonceLen)
	if !ed25519.Verify(ed25519.PublicKey(pub), nonce, s.SignNonce(nonce)) {
		t.Fatal("SignNonce did not verify against HostPublicKey")
	}
}

// TestOpenStoreMigratesSharedSecretFile pins the upgrade path from the
// retired shared-secret model: the old root is dropped, a host key is
// minted, and network consents survive.
func TestOpenStoreMigratesSharedSecretFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "bridge.json")
	v1 := `{
  "secret": "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE",
  "created_at": "2026-07-01T00:00:00Z",
  "first_connected_at": "2026-07-02T00:00:00Z",
  "trusted_networks": {"fp-home": {"added_at": "2026-07-01T00:00:00Z", "label": "home wifi"}}
}`
	if err := os.WriteFile(path, []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore on v1 file: %v", err)
	}
	if len(s.HostPublicKey()) != KeyLen {
		t.Fatal("migration must mint a host key")
	}
	if !s.NetworkTrusted("fp-home") {
		t.Fatal("migration must preserve network consents")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("first_connected_at")) || bytes.Contains(raw, []byte(`"secret"`)) {
		t.Fatalf("migrated file still carries retired fields: %s", raw)
	}
}

func TestDeviceRegistry(t *testing.T) {
	t.Parallel()
	s, path := testStore(t)
	_, pubA := devKey(t, 0x01)
	_, pubB := devKey(t, 0x02)

	if _, ok := s.Device(pubA); ok {
		t.Fatal("fresh store must have no devices")
	}
	if err := s.AddDevice(pubA, "Pixel 8"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddDevice(pubB, "iPhone"); err != nil {
		t.Fatal(err)
	}
	rec, ok := s.Device(pubA)
	if !ok || rec.Name != "Pixel 8" {
		t.Fatalf("Device(A) = %+v %v", rec, ok)
	}

	// Upsert by key: re-approval refreshes the record, no duplicate.
	if err := s.AddDevice(pubA, "Pixel 8 Pro"); err != nil {
		t.Fatal(err)
	}
	if got := len(s.Devices()); got != 2 {
		t.Fatalf("devices = %d, want 2 (upsert must not duplicate)", got)
	}
	if rec, _ := s.Device(pubA); rec.Name != "Pixel 8 Pro" {
		t.Fatalf("upsert name = %q", rec.Name)
	}

	// Persistence: reopen sees both.
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(s2.Devices()); got != 2 {
		t.Fatalf("reopened devices = %d, want 2", got)
	}

	// Remove one; the other survives.
	removed, err := s.RemoveDevice(pubA)
	if err != nil || !removed {
		t.Fatalf("RemoveDevice = %v %v", removed, err)
	}
	if removed, _ := s.RemoveDevice(pubA); removed {
		t.Fatal("second remove must report absent")
	}
	if _, ok := s.Device(pubB); !ok {
		t.Fatal("RemoveDevice(A) must not touch B")
	}

	// Remove all.
	if err := s.AddDevice(pubA, "Pixel 8"); err != nil {
		t.Fatal(err)
	}
	n, err := s.RemoveAllDevices()
	if err != nil || n != 2 {
		t.Fatalf("RemoveAllDevices = %d %v, want 2", n, err)
	}
	s3, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(s3.Devices()); got != 0 {
		t.Fatalf("reopened devices after wipe = %d, want 0", got)
	}
	if !bytes.Equal(s3.HostPublicKey(), s.HostPublicKey()) {
		t.Fatal("RemoveAllDevices must keep the host key — returning phones still recognize the laptop")
	}
}

func TestAddDeviceRejectsBadKey(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t)
	if err := s.AddDevice([]byte("short"), "x"); err == nil {
		t.Fatal("AddDevice with a bad key must error")
	}
}

func TestTouchDevicePersists(t *testing.T) {
	t.Parallel()
	s, path := testStore(t)
	_, pub := devKey(t, 0x03)
	if err := s.AddDevice(pub, "Pixel 8"); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchDevice(pub); err != nil {
		t.Fatal(err)
	}
	rec, _ := s.Device(pub)
	if rec.LastSeen == nil {
		t.Fatal("touch must set last_seen")
	}
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if rec, _ := s2.Device(pub); rec.LastSeen == nil {
		t.Fatal("touched last_seen must persist (debounce off in tests)")
	}
	if err := s.TouchDevice(bytes.Repeat([]byte{0x7F}, KeyLen)); err == nil {
		t.Fatal("touching an unknown device must error")
	}
}

func TestNetworkTrust(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t)
	if s.NetworkTrusted("fp-x") {
		t.Fatal("unknown network must not be trusted")
	}
	if s.NetworkTrusted("") {
		t.Fatal("empty fingerprint must never be trusted")
	}
	if err := s.TrustNetwork("", "nope"); err == nil {
		t.Fatal("trusting an empty fingerprint must error")
	}
	if err := s.TrustNetwork("fp-x", "cafe"); err != nil {
		t.Fatal(err)
	}
	if !s.NetworkTrusted("fp-x") {
		t.Fatal("trusted network not reported")
	}
}

func TestOpenStoreRejectsCorruptHostKey(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "bridge.json")
	if err := os.WriteFile(path, []byte(`{"host_key":"tooshort"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(path); err == nil {
		t.Fatal("corrupt host key must error, not silently re-mint")
	}
}
