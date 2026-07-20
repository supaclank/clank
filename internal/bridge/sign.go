// Package bridge is the daemon's durable laptop↔phone connection,
// built on per-device public keys — nothing secret ever crosses the
// wire. The laptop holds one Ed25519 host keypair (its identity: the
// public key rides the pairing QR, and probes are answered by signing
// the phone's nonce) plus a registry of approved phone public keys.
// Phones sign every request; the daemon verifies against the registry
// and a nonce cache kills replays.
//
// The wire/crypto contract here is mirrored byte-for-byte in
// clank-mobile (src/lib/bridgeCrypto.ts) — change one only with the
// other, and keep the shared test vectors in sync.
package bridge

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// Signed-request headers. Every authenticated bridge request carries
// all four; the signature covers the canonical request string below.
const (
	// HeaderKey is the phone's Ed25519 public key, base64url-nopad.
	HeaderKey = "X-Clank-Key"
	// HeaderTimestamp is unix seconds, decimal — freshness bound.
	HeaderTimestamp = "X-Clank-Ts"
	// HeaderNonce is 16 random bytes, lowercase hex — one-time.
	HeaderNonce = "X-Clank-Nonce"
	// HeaderSignature is the Ed25519 signature, base64url-nopad.
	HeaderSignature = "X-Clank-Sig"
)

const (
	// KeyLen is the Ed25519 seed/public-key size shared by the host
	// key, device keys, and the QR's hk param.
	KeyLen = 32

	// sigVersion pins the canonical-string layout; bump only with a
	// coordinated phone release.
	sigVersion = "clank-sig-v1"

	// sigNonceLen is the exact request-nonce size (bytes, pre-hex).
	sigNonceLen = 16
)

// EncodeKey renders a 32-byte key (host or device public key) for QR
// links and headers: base64url without padding.
func EncodeKey(key []byte) string {
	return base64.RawURLEncoding.EncodeToString(key)
}

// DecodeKey parses an encoded public key, enforcing exact length.
func DecodeKey(s string) ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("bridge: decode key: %w", err)
	}
	if len(raw) != KeyLen {
		return nil, fmt.Errorf("bridge: key must be %d bytes, got %d", KeyLen, len(raw))
	}
	return raw, nil
}

// EncodeSig renders an Ed25519 signature: base64url without padding.
func EncodeSig(sig []byte) string {
	return base64.RawURLEncoding.EncodeToString(sig)
}

// DecodeSig parses an encoded signature, enforcing exact length.
func DecodeSig(s string) ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("bridge: decode signature: %w", err)
	}
	if len(raw) != ed25519.SignatureSize {
		return nil, fmt.Errorf("bridge: signature must be %d bytes, got %d", ed25519.SignatureSize, len(raw))
	}
	return raw, nil
}

// CanonicalRequest is the exact byte string a request signature covers.
// Tampering with any component — freshness, nonce, method, target, or
// body — breaks the signature.
func CanonicalRequest(ts int64, nonceHex, method, requestURI string, body []byte) []byte {
	bodySum := sha256.Sum256(body)
	return []byte(strings.Join([]string{
		sigVersion,
		strconv.FormatInt(ts, 10),
		nonceHex,
		method,
		requestURI,
		hex.EncodeToString(bodySum[:]),
	}, "\n"))
}

// SignRequest produces the HeaderSignature value for a request — the
// client half of the contract, also used by tests and the probe's
// verification path in reverse.
func SignRequest(priv ed25519.PrivateKey, ts int64, nonceHex, method, requestURI string, body []byte) string {
	return EncodeSig(ed25519.Sign(priv, CanonicalRequest(ts, nonceHex, method, requestURI, body)))
}
