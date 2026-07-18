package bridge

import (
	"bytes"
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
	return s, path
}

func TestOpenStoreMintsAndPersists(t *testing.T) {
	t.Parallel()
	s, path := testStore(t)

	root := s.Root()
	if len(root) != RootSecretLen {
		t.Fatalf("root length = %d, want %d", len(root), RootSecretLen)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("bridge.json mode = %o, want 600", perm)
	}

	// Reopen loads the same secret — the credential survives restarts.
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if !bytes.Equal(s2.Root(), root) {
		t.Fatalf("reopened root differs from minted root")
	}
}

func TestRotateRevokesAndRearmsLatch(t *testing.T) {
	t.Parallel()
	s, path := testStore(t)
	old := s.Root()
	if err := s.MarkConnected(); err != nil {
		t.Fatal(err)
	}
	if err := s.TrustNetwork("fp-home", "home wifi"); err != nil {
		t.Fatal(err)
	}

	if err := s.Rotate(); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if bytes.Equal(s.Root(), old) {
		t.Fatal("rotate did not change the secret")
	}
	if s.FirstConnected() {
		t.Error("rotate must clear first_connected_at (QR re-embeds the token)")
	}
	if !s.NetworkTrusted("fp-home") {
		t.Error("rotate must preserve network consents — they're about the LAN, not the phones")
	}

	// Persisted: reopen sees rotated state.
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(s2.Root(), old) || s2.FirstConnected() || !s2.NetworkTrusted("fp-home") {
		t.Fatal("rotated state did not persist")
	}
}

func TestMarkConnectedLatches(t *testing.T) {
	t.Parallel()
	s, path := testStore(t)
	if s.FirstConnected() {
		t.Fatal("fresh store must not be marked connected")
	}
	if err := s.MarkConnected(); err != nil {
		t.Fatal(err)
	}
	if !s.FirstConnected() {
		t.Fatal("latch did not set")
	}
	if err := s.MarkConnected(); err != nil {
		t.Fatalf("second MarkConnected must be a no-op, got %v", err)
	}
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.FirstConnected() {
		t.Fatal("latch did not persist")
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

func TestOpenStoreRejectsCorruptSecret(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "bridge.json")
	if err := os.WriteFile(path, []byte(`{"secret":"tooshort"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(path); err == nil {
		t.Fatal("corrupt secret must error, not silently re-mint")
	}
}
