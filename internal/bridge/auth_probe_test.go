package bridge

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/supaclank/clank/pkg/auth"
)

// reqBuilder signs one request honestly, then optionally tampers with
// what's actually sent — the signature stops covering reality.
type reqBuilder struct {
	priv       ed25519.PrivateKey
	ts         int64
	nonce      string
	method     string
	uri        string
	body       string
	tamperBody string
	tamperURI  string
	garbageSig bool
}

func (b *reqBuilder) request() *http.Request {
	sig := SignRequest(b.priv, b.ts, b.nonce, b.method, b.uri, []byte(b.body))
	if b.garbageSig {
		sig = EncodeSig(make([]byte, ed25519.SignatureSize))
	}
	sentURI, sentBody := b.uri, b.body
	if b.tamperURI != "" {
		sentURI = b.tamperURI
	}
	if b.tamperBody != "" {
		sentBody = b.tamperBody
	}
	r := httptest.NewRequest(b.method, sentURI, strings.NewReader(sentBody))
	r.Header.Set(HeaderKey, EncodeKey(b.priv.Public().(ed25519.PublicKey)))
	r.Header.Set(HeaderTimestamp, strconv.FormatInt(b.ts, 10))
	r.Header.Set(HeaderNonce, b.nonce)
	r.Header.Set(HeaderSignature, sig)
	return r
}

func TestSignedRequestAuth(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t)
	dev := vectorKey(t, vectorDevSeedB64)
	devPub := dev.Public().(ed25519.PublicKey)
	if err := s.AddDevice(devPub, "Pixel 8"); err != nil {
		t.Fatal(err)
	}
	a := NewAuthenticator(s, "axel", nil, func() time.Time { return time.Unix(vectorTS, 0) })

	b := &reqBuilder{priv: dev, ts: vectorTS, nonce: "10101010101010101010101010101010",
		method: "POST", uri: "/v1/sessions", body: `{"x":1}`}
	r := b.request()
	p, err := a.Verify(r)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if p.UserID != "axel" {
		t.Errorf("UserID = %q, want axel", p.UserID)
	}
	// The body must be readable downstream after verification consumed it.
	got, _ := io.ReadAll(r.Body)
	if string(got) != `{"x":1}` {
		t.Fatalf("body after Verify = %q", got)
	}
	// Success recorded: last connection + registry last_seen.
	if device, at := a.LastConnection(); device != "Pixel 8" || at.IsZero() {
		t.Fatalf("LastConnection = %q %v", device, at)
	}
	if rec, _ := s.Device(devPub); rec.LastSeen == nil {
		t.Fatal("Verify must touch the device's last_seen")
	}
}

func TestSignedRequestAuthRejects(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t)
	dev := vectorKey(t, vectorDevSeedB64)
	devPub := dev.Public().(ed25519.PublicKey)
	stranger := vectorKey(t, vectorHostSeedB64) // valid key, never registered
	if err := s.AddDevice(devPub, "Pixel 8"); err != nil {
		t.Fatal(err)
	}
	a := NewAuthenticator(s, "axel", nil, func() time.Time { return time.Unix(vectorTS, 0) })

	nonceN := 0
	build := func(priv ed25519.PrivateKey, mutate func(b *reqBuilder)) *reqBuilder {
		nonceN++
		b := &reqBuilder{priv: priv, ts: vectorTS, nonce: fmt.Sprintf("%032x", nonceN),
			method: "GET", uri: "/v1/repos"}
		if mutate != nil {
			mutate(b)
		}
		return b
	}

	cases := []struct {
		name string
		b    *reqBuilder
	}{
		{"unregistered key", build(stranger, nil)},
		{"stale timestamp", build(dev, func(b *reqBuilder) { b.ts = vectorTS - 300 })},
		{"future timestamp", build(dev, func(b *reqBuilder) { b.ts = vectorTS + 300 })},
		{"tampered body", build(dev, func(b *reqBuilder) { b.method = "POST"; b.body = `{"x":1}`; b.tamperBody = `{"evil":1}` })},
		{"tampered path", build(dev, func(b *reqBuilder) { b.tamperURI = "/v1/hosts" })},
		{"garbage signature", build(dev, func(b *reqBuilder) { b.garbageSig = true })},
	}
	for _, tc := range cases {
		if _, err := a.Verify(tc.b.request()); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("%s: got %v, want ErrUnauthenticated", tc.name, err)
		}
	}

	// Missing headers entirely.
	if _, err := a.Verify(httptest.NewRequest("GET", "/", nil)); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("bare request: got %v", err)
	}

	// Replay: the same signed request verbatim must fail the second time.
	replay := build(dev, nil)
	if _, err := a.Verify(replay.request()); err != nil {
		t.Fatalf("first send: %v", err)
	}
	if _, err := a.Verify(replay.request()); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("replay: got %v, want ErrUnauthenticated", err)
	}

	// Revocation: removing the device kills its next request.
	ok := build(dev, nil)
	if _, err := s.RemoveDevice(devPub); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Verify(ok.request()); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("revoked device: got %v, want ErrUnauthenticated", err)
	}
}

func TestBadSignatureDoesNotBurnNonce(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t)
	dev := vectorKey(t, vectorDevSeedB64)
	devPub := dev.Public().(ed25519.PublicKey)
	if err := s.AddDevice(devPub, "Pixel 8"); err != nil {
		t.Fatal(err)
	}
	a := NewAuthenticator(s, "axel", nil, func() time.Time { return time.Unix(vectorTS, 0) })

	// An unauthenticated attacker who only knows the (public) device key
	// sends garbage signatures to flood the nonce cache.
	nonce := "20202020202020202020202020202020"
	bad := &reqBuilder{priv: dev, ts: vectorTS, nonce: nonce, method: "GET", uri: "/v1/repos", garbageSig: true}
	if _, err := a.Verify(bad.request()); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("bad signature: got %v, want ErrUnauthenticated", err)
	}

	// The same nonce, now genuinely signed, must still succeed — a
	// rejected signature must not have reserved (burned) the nonce.
	good := &reqBuilder{priv: dev, ts: vectorTS, nonce: nonce, method: "GET", uri: "/v1/repos"}
	if _, err := a.Verify(good.request()); err != nil {
		t.Fatalf("genuine signature after a bad one on the same nonce: %v", err)
	}
}

func TestSessionTokenExpiry(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t)
	dev := vectorKey(t, vectorDevSeedB64)
	devPub := dev.Public().(ed25519.PublicKey)
	if err := s.AddDevice(devPub, "Pixel 8"); err != nil {
		t.Fatal(err)
	}
	clk := newClock()
	a := NewAuthenticator(s, "axel", nil, clk.now)

	token, _, err := a.MintSessionToken(devPub)
	if err != nil {
		t.Fatalf("MintSessionToken: %v", err)
	}
	bearer := func() *http.Request {
		r := httptest.NewRequest("GET", "/v1/repos", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		return r
	}
	if _, err := a.Verify(bearer()); err != nil {
		t.Fatalf("fresh token: %v", err)
	}

	clk.advance(sessionTokenTTL + time.Second)
	if _, err := a.Verify(bearer()); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("expired token: got %v, want ErrUnauthenticated", err)
	}
}

func TestSanitizeDeviceNameTruncatesOnRuneBoundary(t *testing.T) {
	t.Parallel()
	// 70 multi-byte runes: a byte-index truncation at 64 would split
	// the 65th rune's UTF-8 encoding in half.
	name := strings.Repeat("é", 70)
	got := sanitizeDeviceName(name)
	if !utf8.ValidString(got) {
		t.Fatalf("sanitizeDeviceName(%d runes) = %q, not valid UTF-8", len([]rune(name)), got)
	}
	if want := strings.Repeat("é", 64); got != want {
		t.Fatalf("sanitizeDeviceName(%d runes) = %q, want %q", len([]rune(name)), got, want)
	}
}

func TestProbeHandlerProvesIdentity(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t)
	h := ProbeHandler(s)

	nonce := vectorNonce()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/bridge/ping?nonce="+hex.EncodeToString(nonce), nil))
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body struct {
		Sig  string `json:"sig"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}

	// Verify the answer the way the phone does: the host key's
	// signature over the nonce, checked against the QR-learned pubkey.
	sig, err := DecodeSig(body.Sig)
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(ed25519.PublicKey(s.HostPublicKey()), nonce, sig) {
		t.Fatal("probe signature did not verify against the host public key")
	}
}

func TestProbeHandlerRejectsBadNonces(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t)
	h := ProbeHandler(s)
	for _, q := range []string{"", "nonce=zz", "nonce=00ff", "nonce=" + hex.EncodeToString(make([]byte, 32))} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/bridge/ping?"+q, nil))
		if w.Code != 400 {
			t.Errorf("query %q: status = %d, want 400", q, w.Code)
		}
	}
}
