package bridge

import (
	"bytes"
	"testing"
)

// Shared cross-implementation vectors: clank-mobile's
// src/lib/__tests__/bridgeCrypto.test.ts asserts these exact values.
// If this test changes, that one must change in the same release —
// drift here means phones silently fail to authenticate.
const (
	vectorTok    = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"
	vectorBearer = "clankb_AyXyRBlfj0pkkoUTC3pe_dMPi_GK_aN4Yqhu692S6ao"
	vectorProof  = "3d6eec35917b91bf5d6bcc027a6a1278daa841fee7f0ff1f7678d18aad6533b7"
)

func vectorRoot() []byte { return bytes.Repeat([]byte{0x01}, RootSecretLen) }

func vectorNonce() []byte {
	nonce := make([]byte, 16)
	for i := range nonce {
		nonce[i] = byte(i)
	}
	return nonce
}

func TestSharedVectors(t *testing.T) {
	t.Parallel()
	root := vectorRoot()

	if got := EncodeRoot(root); got != vectorTok {
		t.Errorf("EncodeRoot = %s, want %s", got, vectorTok)
	}
	bearer, err := BearerString(root)
	if err != nil {
		t.Fatalf("BearerString: %v", err)
	}
	if bearer != vectorBearer {
		t.Errorf("BearerString = %s, want %s", bearer, vectorBearer)
	}
	proof, err := Proof(root, vectorNonce())
	if err != nil {
		t.Fatalf("Proof: %v", err)
	}
	if proof != vectorProof {
		t.Errorf("Proof = %s, want %s", proof, vectorProof)
	}
}

func TestRootRoundTrip(t *testing.T) {
	t.Parallel()
	root := vectorRoot()
	decoded, err := DecodeRoot(EncodeRoot(root))
	if err != nil {
		t.Fatalf("DecodeRoot: %v", err)
	}
	if !bytes.Equal(decoded, root) {
		t.Fatalf("round-trip mismatch")
	}
}

func TestDecodeRootRejectsBadInput(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "!!!", "AQEB", vectorTok + "AA"} {
		if _, err := DecodeRoot(in); err == nil {
			t.Errorf("DecodeRoot(%q): expected error", in)
		}
	}
}

// TestSubkeysDiffer pins that bearer and identity keys are
// domain-separated — deriving both from the root must never yield the
// same bytes (a shared subkey would let a probe response double as a
// credential).
func TestSubkeysDiffer(t *testing.T) {
	t.Parallel()
	root := vectorRoot()
	bearerKey, err := deriveKey(root, infoBearer)
	if err != nil {
		t.Fatal(err)
	}
	idKey, err := deriveKey(root, infoIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(bearerKey, idKey) {
		t.Fatal("bearer and identity subkeys are identical")
	}
}
