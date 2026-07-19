package bridge

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
)

// The SAS pairing handshake authenticates WHICH device public key the
// laptop enrolls, closing the active-MITM hole at pairing without a
// TLS pin. It's the Vaudenay short-authentication-string model plus the
// optical channel we already have:
//
//   - commit-then-reveal: the phone commits to (device key ‖ nonce)
//     before it sees the daemon's nonce, so no side can grind its
//     contribution to force a SAS collision after the fact — that's
//     what makes 6 digits enough (grinding is online-only, throttled by
//     the window lease + pending cap + lockout).
//   - the daemon signs its reply with the host key; the phone verifies
//     against the hk it learned from the QR BEFORE showing anything, so
//     a MITM is caught immediately in the daemon→phone direction and
//     forced to relay the real commit.
//   - both sides derive the same 6-digit SAS from the full transcript;
//     the phone displays it (never sent on the wire), the user types it
//     at the laptop, and that authenticates the phone→laptop direction.
//
// The whole contract is mirrored byte-for-byte in clank-mobile
// (src/lib/bridgeCrypto.ts) — shared vectors in sas_test.go ↔
// bridgeCrypto.test.ts. No ECDH: we authenticate a datum (the pubkey),
// we don't establish a channel.
const (
	// sasVersion pins the transcript layout; bump only with a
	// coordinated phone release.
	sasVersion = "clank-sas-v1"

	// SASDigits is the length of the human-typed code.
	SASDigits = 6

	// sasNonceLen is the exact commit/reply nonce size (bytes).
	sasNonceLen = 16
)

// SASCommit hashes the device public key and the phone's nonce into the
// opaque commitment the phone sends first. Collision resistance is what
// stops a MITM from later opening the commit to a different key.
func SASCommit(devicePub, nonceP []byte) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		sasVersion, "commit", EncodeKey(devicePub), hex.EncodeToString(nonceP),
	}, "\n")))
	return hex.EncodeToString(sum[:])
}

// sasReplyMessage is what the host key signs: it binds the daemon's
// nonce to THIS phone's commit, so a MITM can't splice a signed reply
// onto a different commitment.
func sasReplyMessage(attemptID, commitHex string, nonceD []byte) []byte {
	return []byte(strings.Join([]string{
		sasVersion, "reply", attemptID, commitHex, hex.EncodeToString(nonceD),
	}, "\n"))
}

// VerifySASReply checks the daemon's reply signature against the host
// public key the phone learned from the QR — the phone's parity with
// the daemon, exercised here so the shared vectors cover both sides.
func VerifySASReply(hostPub []byte, attemptID, commitHex string, nonceD []byte, sigB64 string) bool {
	sig, err := DecodeSig(sigB64)
	if err != nil || len(hostPub) != KeyLen {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(hostPub), sasReplyMessage(attemptID, commitHex, nonceD), sig)
}

// DeriveSAS derives the 6-digit code from the full transcript — both
// nonces, the committed device key, and the host key. Identical on both
// sides; the phone displays it, the laptop user types it.
func DeriveSAS(attemptID, commitHex string, nonceD, devicePub, nonceP, hostPub []byte) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		sasVersion, "sas", attemptID, commitHex,
		hex.EncodeToString(nonceD), EncodeKey(devicePub), hex.EncodeToString(nonceP), EncodeKey(hostPub),
	}, "\n")))
	// mod 10^6 carries a negligible bias (2^32 not a multiple of 10^6);
	// same construction as Bluetooth SSP numeric comparison.
	n := binary.BigEndian.Uint32(sum[:4]) % 1_000_000
	return fmt.Sprintf("%0*d", SASDigits, n)
}
