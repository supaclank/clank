// Package bridge is the daemon's durable laptop↔phone connection: one
// root secret per laptop (delivered once via QR), HKDF-split into a
// bearer credential the phone presents and an identity key the laptop
// proves itself with on every probe. The phone re-finds the laptop by
// probing remembered candidate addresses; the proof check is what
// keeps a remembered-but-reassigned address from harvesting the
// bearer.
//
// The wire/crypto contract here is mirrored byte-for-byte in
// clank-mobile (src/lib/bridgeCrypto.ts) — change one only with the
// other, and keep the shared test vectors in sync.
package bridge

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

const (
	// RootSecretLen is the size of the laptop's root secret.
	RootSecretLen = 32

	// hkdfSalt versions the derivation; bump only with a coordinated
	// phone release (it invalidates every stored pairing).
	hkdfSalt = "clank-bridge-v1"

	infoBearer   = "bearer"
	infoIdentity = "identity"

	// bearerPrefix marks bridge bearers in logs and headers without
	// revealing them.
	bearerPrefix = "clankb_"
)

// deriveKey is the shared HKDF-SHA256 expansion for both subkeys.
func deriveKey(root []byte, info string) ([]byte, error) {
	if len(root) != RootSecretLen {
		return nil, fmt.Errorf("bridge: root secret must be %d bytes, got %d", RootSecretLen, len(root))
	}
	return hkdf.Key(sha256.New, root, []byte(hkdfSalt), info, RootSecretLen)
}

// BearerString derives the Authorization bearer the phone presents:
// "clankb_" + base64url-nopad(HKDF(root, "bearer")).
func BearerString(root []byte) (string, error) {
	key, err := deriveKey(root, infoBearer)
	if err != nil {
		return "", err
	}
	return bearerPrefix + base64.RawURLEncoding.EncodeToString(key), nil
}

// identityKey derives the key the laptop proves itself with on probes.
func identityKey(root []byte) ([]byte, error) {
	return deriveKey(root, infoIdentity)
}

// Proof answers a probe nonce: hex(HMAC-SHA256(identityKey, nonce)).
func Proof(root, nonce []byte) (string, error) {
	key, err := identityKey(root)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(nonce)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// EncodeRoot renders the root secret for QR links (the `tok` param):
// base64url without padding.
func EncodeRoot(root []byte) string {
	return base64.RawURLEncoding.EncodeToString(root)
}

// DecodeRoot parses a QR-carried root secret, enforcing exact length.
func DecodeRoot(tok string) ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return nil, fmt.Errorf("bridge: decode root secret: %w", err)
	}
	if len(raw) != RootSecretLen {
		return nil, fmt.Errorf("bridge: root secret must be %d bytes, got %d", RootSecretLen, len(raw))
	}
	return raw, nil
}
