package bridge

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"testing"
)

// Shared cross-implementation vectors. clank-mobile's
// src/lib/__tests__/bridgeCrypto.test.ts asserts these exact values —
// the two implementations cannot drift silently. Change only together.
const (
	vectorHostSeedB64 = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE" // 32×0x01
	vectorHostPubB64  = "iojj3XQJ8ZX9UtstPLpdcspnCb8dlBIb83SIAbQPb1w"
	vectorDevSeedB64  = "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI" // 32×0x02
	vectorDevPubB64   = "gTl3Dqh9F19Wo1Rmw0x-zMuNipG07jeiXfYPW4_Js5Q"

	vectorNonceHex    = "000102030405060708090a0b0c0d0e0f"
	vectorProbeSigB64 = "A3Bm24TROJbbjGD2R3H8N1u-nU_KBhfTGCo1_UCjJxAIxmJSDVfq9muo9Z2_Bu6OmzPuMjFeBkv7ocTzmpWyDA"
	vectorTS          = int64(1752940000)
	vectorPostURI     = "/v1/repos?limit=2"
	vectorPostBody    = `{"q":"clank"}`
	vectorPostSigB64  = "7I4t1war7AM6xzdwmFKG4DkviIKFN1Eu5pykLCPyqDmQB9l_g42xTG63hk_z1xZ_x_F9Oeu5am03ycm8BIBFCw"
	vectorGetURI      = "/v1/repos"
	vectorGetSigB64   = "Hxielf3pL4IQXD1Ef4_5iESwTD_mBGGqKsYP3MHbvurbjLWfOcZSqsCyJT89QMdyzHp5fAHtk5RRJ48Gi_DxCQ"
	vectorCanonical   = "clank-sig-v1\n1752940000\n000102030405060708090a0b0c0d0e0f\nPOST\n/v1/repos?limit=2\n6440ba6c7d76f67e8ac77109db5635fab0d0dbe82917815897139967558cce28"
)

func vectorNonce() []byte {
	raw, _ := hex.DecodeString(vectorNonceHex)
	return raw
}

func vectorKey(t *testing.T, seedB64 string) ed25519.PrivateKey {
	t.Helper()
	seed, err := DecodeKey(seedB64)
	if err != nil {
		t.Fatalf("vector seed: %v", err)
	}
	return ed25519.NewKeyFromSeed(seed)
}

func TestVectorKeys(t *testing.T) {
	t.Parallel()
	host := vectorKey(t, vectorHostSeedB64)
	if got := EncodeKey(host.Public().(ed25519.PublicKey)); got != vectorHostPubB64 {
		t.Fatalf("host pub = %s, want %s", got, vectorHostPubB64)
	}
	dev := vectorKey(t, vectorDevSeedB64)
	if got := EncodeKey(dev.Public().(ed25519.PublicKey)); got != vectorDevPubB64 {
		t.Fatalf("dev pub = %s, want %s", got, vectorDevPubB64)
	}
}

func TestVectorProbeSignature(t *testing.T) {
	t.Parallel()
	host := vectorKey(t, vectorHostSeedB64)
	if got := EncodeSig(ed25519.Sign(host, vectorNonce())); got != vectorProbeSigB64 {
		t.Fatalf("probe sig = %s, want %s", got, vectorProbeSigB64)
	}
}

func TestVectorCanonicalRequestAndSignatures(t *testing.T) {
	t.Parallel()
	c := CanonicalRequest(vectorTS, vectorNonceHex, "POST", vectorPostURI, []byte(vectorPostBody))
	if string(c) != vectorCanonical {
		t.Fatalf("canonical =\n%q\nwant\n%q", c, vectorCanonical)
	}
	dev := vectorKey(t, vectorDevSeedB64)
	if got := SignRequest(dev, vectorTS, vectorNonceHex, "POST", vectorPostURI, []byte(vectorPostBody)); got != vectorPostSigB64 {
		t.Fatalf("post sig = %s, want %s", got, vectorPostSigB64)
	}
	if got := SignRequest(dev, vectorTS, vectorNonceHex, "GET", vectorGetURI, nil); got != vectorGetSigB64 {
		t.Fatalf("get sig = %s, want %s", got, vectorGetSigB64)
	}
}

func TestKeyAndSigCodecs(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{0x5A}, KeyLen)
	round, err := DecodeKey(EncodeKey(key))
	if err != nil || !bytes.Equal(round, key) {
		t.Fatalf("key round-trip = %x %v", round, err)
	}
	for _, bad := range []string{"", "!!!", EncodeKey(key[:16])} {
		if _, err := DecodeKey(bad); err == nil {
			t.Errorf("DecodeKey(%q) must error", bad)
		}
	}
	sig := bytes.Repeat([]byte{0x5B}, ed25519.SignatureSize)
	roundSig, err := DecodeSig(EncodeSig(sig))
	if err != nil || !bytes.Equal(roundSig, sig) {
		t.Fatalf("sig round-trip = %x %v", roundSig, err)
	}
	if _, err := DecodeSig(EncodeKey(key)); err == nil {
		t.Error("DecodeSig must enforce signature length")
	}
}
