package bridge

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// TrustedNetwork records a per-network LAN consent ("trust this LAN?"
// answered yes), keyed in the store by the network fingerprint
// (netid.go). Label is a best-effort human hint for `clank pair
// status`, never an approval input.
type TrustedNetwork struct {
	AddedAt time.Time `json:"added_at"`
	Label   string    `json:"label,omitempty"`
}

// stateFile is the on-disk shape of bridge.json.
type stateFile struct {
	// Secret is base64url-nopad of the 32-byte root. Plaintext at
	// rest deliberately: the daemon must re-display the pairing QR
	// and answer identity proofs, both of which need the actual
	// secret — the ssh-host-key posture (0600; leaked file ⇒ rotate).
	Secret           string                    `json:"secret"`
	CreatedAt        time.Time                 `json:"created_at"`
	RotatedAt        *time.Time                `json:"rotated_at,omitempty"`
	FirstConnectedAt *time.Time                `json:"first_connected_at,omitempty"`
	TrustedNetworks  map[string]TrustedNetwork `json:"trusted_networks,omitempty"`
}

// Store owns bridge.json: the root secret, the first-connection latch
// that stops embedding the secret in preview QRs, and the per-network
// LAN consents. Safe for concurrent use.
type Store struct {
	mu    sync.Mutex
	path  string
	state stateFile
	root  []byte
}

// OpenStore loads bridge.json at path, minting a fresh root secret
// (and the file) when none exists.
func OpenStore(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("bridge: store path is required")
	}
	s := &Store{path: path}
	raw, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		if err := s.mintLocked(); err != nil {
			return nil, err
		}
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("bridge: read %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, &s.state); err != nil {
		return nil, fmt.Errorf("bridge: parse %s: %w", path, err)
	}
	root, err := base64.RawURLEncoding.DecodeString(s.state.Secret)
	if err != nil || len(root) != RootSecretLen {
		return nil, fmt.Errorf("bridge: %s holds an invalid secret — delete it to re-pair", path)
	}
	s.root = root
	return s, nil
}

// Root returns a copy of the root secret.
func (s *Store) Root() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]byte, len(s.root))
	copy(out, s.root)
	return out
}

// Rotate mints a new root secret, disconnecting every phone (they
// hold derivations of the old one) and re-arming the QR token embed
// (first_connected_at clears). Network consents survive — they're
// about the LAN, not the phones.
func (s *Store) Rotate() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	trusted := s.state.TrustedNetworks
	created := s.state.CreatedAt
	if err := s.mintLocked(); err != nil {
		return err
	}
	s.state.CreatedAt = created
	s.state.RotatedAt = &now
	s.state.TrustedNetworks = trusted
	return s.persistLocked()
}

// FirstConnected reports whether any phone has ever authenticated —
// the switch that turns preview QRs from credential-bearing (first
// run) into tokenless invitations.
func (s *Store) FirstConnected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.FirstConnectedAt != nil
}

// MarkConnected latches first_connected_at. Idempotent; persists only
// on the first call after mint/rotate.
func (s *Store) MarkConnected() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.FirstConnectedAt != nil {
		return nil
	}
	now := time.Now().UTC()
	s.state.FirstConnectedAt = &now
	return s.persistLocked()
}

// NetworkTrusted reports whether the fingerprinted network has been
// consented to for plain-LAN serving. Empty fingerprints (detection
// failed) are never trusted.
func (s *Store) NetworkTrusted(fingerprint string) bool {
	if fingerprint == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.state.TrustedNetworks[fingerprint]
	return ok
}

// TrustNetwork records LAN consent for the fingerprinted network.
func (s *Store) TrustNetwork(fingerprint, label string) error {
	if fingerprint == "" {
		return fmt.Errorf("bridge: cannot trust an unidentified network")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.TrustedNetworks == nil {
		s.state.TrustedNetworks = make(map[string]TrustedNetwork)
	}
	s.state.TrustedNetworks[fingerprint] = TrustedNetwork{AddedAt: time.Now().UTC(), Label: label}
	return s.persistLocked()
}

// mintLocked replaces the in-memory state with a freshly generated
// secret and persists. Caller holds s.mu (or is the constructor).
func (s *Store) mintLocked() error {
	root := make([]byte, RootSecretLen)
	if _, err := rand.Read(root); err != nil {
		return fmt.Errorf("bridge: generate secret: %w", err)
	}
	s.root = root
	s.state = stateFile{
		Secret:    base64.RawURLEncoding.EncodeToString(root),
		CreatedAt: time.Now().UTC(),
	}
	return s.persistLocked()
}

// persistLocked writes bridge.json atomically at 0600 — same posture
// as the daemon's other credential files. Caller holds s.mu.
func (s *Store) persistLocked() error {
	payload, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("bridge: marshal state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("bridge: state dir: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o600); err != nil {
		return fmt.Errorf("bridge: write state: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("bridge: replace state: %w", err)
	}
	return nil
}
