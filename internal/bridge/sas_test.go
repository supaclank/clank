package bridge

import (
	"crypto/ed25519"
	"encoding/hex"
	"testing"
)

// Shared SAS handshake vectors — clank-mobile's bridgeCrypto.test.ts
// asserts these exact values. Change only together.
const (
	sasVectorCommitHex = "4f90f3d2e8c244cfa2cb176a526759748d41357c1faf417603e4f9647f4fad82"
	sasVectorReplySig  = "H2RaZLBIAsDFMdbjN1mFitaatTQtZaLGirLsKSVFupEXWXtlRechPcy9u-6ZH_2hcfVrjmX3ACwsxNITW65MCA"
	sasVectorSAS       = "626680"
	sasVectorAttemptID = "00112233445566778899aabbccddeeff"
)

func sasVectorNonceP() []byte { return seqBytes(0x10) }
func sasVectorNonceD() []byte { return seqBytes(0x20) }

func seqBytes(start byte) []byte {
	out := make([]byte, sasNonceLen)
	for i := range out {
		out[i] = start + byte(i)
	}
	return out
}

func TestSASCommitVector(t *testing.T) {
	t.Parallel()
	dev := vectorKey(t, vectorDevSeedB64)
	got := SASCommit(dev.Public().(ed25519.PublicKey), sasVectorNonceP())
	if got != sasVectorCommitHex {
		t.Fatalf("commit = %s, want %s", got, sasVectorCommitHex)
	}
}

func TestSASReplySignatureVector(t *testing.T) {
	t.Parallel()
	host := vectorKey(t, vectorHostSeedB64)
	hostPub := host.Public().(ed25519.PublicKey)
	sig := EncodeSig(ed25519.Sign(host, sasReplyMessage(sasVectorAttemptID, sasVectorCommitHex, sasVectorNonceD())))
	if sig != sasVectorReplySig {
		t.Fatalf("reply sig = %s, want %s", sig, sasVectorReplySig)
	}
	// Phone-side parity: VerifySASReply accepts it, rejects tampering.
	if !VerifySASReply(hostPub, sasVectorAttemptID, sasVectorCommitHex, sasVectorNonceD(), sig) {
		t.Fatal("VerifySASReply rejected a valid reply")
	}
	if VerifySASReply(hostPub, sasVectorAttemptID, sasVectorCommitHex, seqBytes(0x21), sig) {
		t.Fatal("VerifySASReply accepted a tampered nonce")
	}
	dev := vectorKey(t, vectorDevSeedB64)
	if VerifySASReply(dev.Public().(ed25519.PublicKey), sasVectorAttemptID, sasVectorCommitHex, sasVectorNonceD(), sig) {
		t.Fatal("VerifySASReply accepted the wrong host key")
	}
}

func TestDeriveSASVector(t *testing.T) {
	t.Parallel()
	host := vectorKey(t, vectorHostSeedB64)
	dev := vectorKey(t, vectorDevSeedB64)
	got := DeriveSAS(
		sasVectorAttemptID, sasVectorCommitHex, sasVectorNonceD(),
		dev.Public().(ed25519.PublicKey), sasVectorNonceP(), host.Public().(ed25519.PublicKey),
	)
	if got != sasVectorSAS {
		t.Fatalf("SAS = %s, want %s", got, sasVectorSAS)
	}
	if len(got) != SASDigits {
		t.Fatalf("SAS length = %d, want %d", len(got), SASDigits)
	}
}

// TestDeriveSASBindsEveryComponent is the security property in test
// form: flip any transcript input and the SAS changes, so a MITM whose
// leg differs in any field derives a different code.
func TestDeriveSASBindsEveryComponent(t *testing.T) {
	t.Parallel()
	host := vectorKey(t, vectorHostSeedB64).Public().(ed25519.PublicKey)
	dev := vectorKey(t, vectorDevSeedB64).Public().(ed25519.PublicKey)
	other := vectorKey(t, vectorHostSeedB64).Public().(ed25519.PublicKey) // reused as a "different key"
	base := DeriveSAS(sasVectorAttemptID, sasVectorCommitHex, sasVectorNonceD(), dev, sasVectorNonceP(), host)

	variants := map[string]string{
		"attemptID": DeriveSAS("ffffffffffffffffffffffffffffffff", sasVectorCommitHex, sasVectorNonceD(), dev, sasVectorNonceP(), host),
		"commit":    DeriveSAS(sasVectorAttemptID, hex.EncodeToString(make([]byte, 32)), sasVectorNonceD(), dev, sasVectorNonceP(), host),
		"nonceD":    DeriveSAS(sasVectorAttemptID, sasVectorCommitHex, seqBytes(0x21), dev, sasVectorNonceP(), host),
		"devicePub": DeriveSAS(sasVectorAttemptID, sasVectorCommitHex, sasVectorNonceD(), other, sasVectorNonceP(), host),
		"nonceP":    DeriveSAS(sasVectorAttemptID, sasVectorCommitHex, sasVectorNonceD(), dev, seqBytes(0x11), host),
		"hostPub":   DeriveSAS(sasVectorAttemptID, sasVectorCommitHex, sasVectorNonceD(), dev, sasVectorNonceP(), dev),
	}
	for field, got := range variants {
		if got == base {
			t.Errorf("changing %s did not change the SAS (%s) — not bound in the transcript", field, got)
		}
	}
}
