package bridge

import (
	"crypto/ed25519"
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
	// HostKey is base64url-nopad of the laptop's 32-byte Ed25519 seed —
	// the file's only secret. Plaintext at rest deliberately: the daemon
	// must answer identity probes, which needs the actual key — the
	// ssh-host-key posture (0600; leaked file ⇒ phones re-pair against a
	// fresh key). Device entries are public keys, so a leaked file can
	// impersonate the laptop to a phone but never a phone to the laptop.
	HostKey         string                    `json:"host_key"`
	CreatedAt       time.Time                 `json:"created_at"`
	Devices         []DeviceRecord            `json:"devices,omitempty"`
	TrustedNetworks map[string]TrustedNetwork `json:"trusted_networks,omitempty"`
}

// Store owns bridge.json: the host identity key, the approved-device
// registry, and the per-network LAN consents. Safe for concurrent use.
type Store struct {
	mu    sync.Mutex
	path  string
	state stateFile
	priv  ed25519.PrivateKey

	// touchFlushInterval debounces LastSeen persistence — signed
	// requests are chatty and last_seen is display state, not a gate.
	// 0 persists every touch (tests).
	touchFlushInterval time.Duration
	lastTouchFlush     time.Time
}

// defaultTouchFlushInterval bounds how stale a persisted last_seen can
// be; in-memory state is always current.
const defaultTouchFlushInterval = time.Minute

// OpenStore loads bridge.json at path, minting a host keypair (and the
// file) when none exists. Files from the retired shared-secret model
// load cleanly: their network consents survive, the old secret is
// dropped, and a host key is minted in its place.
func OpenStore(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("bridge: store path is required")
	}
	s := &Store{path: path, touchFlushInterval: defaultTouchFlushInterval}
	raw, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		if err := s.mintHostKeyLocked(); err != nil {
			return nil, err
		}
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("bridge: read %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, &s.state); err != nil {
		return nil, fmt.Errorf("bridge: parse %s: %w", path, err)
	}
	if s.state.HostKey == "" {
		if err := s.mintHostKeyLocked(); err != nil {
			return nil, err
		}
		return s, nil
	}
	seed, err := base64.RawURLEncoding.DecodeString(s.state.HostKey)
	if err != nil || len(seed) != KeyLen {
		return nil, fmt.Errorf("bridge: %s holds an invalid host key — delete it to re-pair", path)
	}
	s.priv = ed25519.NewKeyFromSeed(seed)
	for _, d := range s.state.Devices {
		if _, err := DecodeKey(d.PubKey); err != nil {
			return nil, fmt.Errorf("bridge: %s holds an invalid device key — delete it to re-pair", path)
		}
	}
	return s, nil
}

// HostPublicKey returns the laptop's identity public key — the QR's hk
// param and the probe verification anchor on the phone.
func (s *Store) HostPublicKey() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	pub := s.priv.Public().(ed25519.PublicKey)
	out := make([]byte, len(pub))
	copy(out, pub)
	return out
}

// SignNonce answers an identity probe: the host key's signature over
// the phone-chosen nonce.
func (s *Store) SignNonce(nonce []byte) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ed25519.Sign(s.priv, nonce)
}

// SignSASReply signs the pairing handshake reply with the host key,
// binding the daemon's nonce to the phone's commit. The phone verifies
// it against the QR's hk before deriving the SAS.
func (s *Store) SignSASReply(attemptID, commitHex string, nonceD []byte) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ed25519.Sign(s.priv, sasReplyMessage(attemptID, commitHex, nonceD))
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

// mintHostKeyLocked generates a fresh host keypair into the current
// state (preserving migrated fields like network consents) and
// persists. Caller holds s.mu (or is the constructor).
func (s *Store) mintHostKeyLocked() error {
	seed := make([]byte, KeyLen)
	if _, err := rand.Read(seed); err != nil {
		return fmt.Errorf("bridge: generate host key: %w", err)
	}
	s.priv = ed25519.NewKeyFromSeed(seed)
	s.state.HostKey = base64.RawURLEncoding.EncodeToString(seed)
	if s.state.CreatedAt.IsZero() {
		s.state.CreatedAt = time.Now().UTC()
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
