package bridge

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/acksell/clank/pkg/auth"
)

func TestAuthenticatorAcceptsDerivedBearerAndLatches(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t)
	a := NewAuthenticator(s, "axel", nil)

	bearer, err := BearerString(s.Root())
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/v1/repos", nil)
	r.Header.Set("Authorization", "Bearer "+bearer)

	p, err := a.Verify(r)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if p.UserID != "axel" {
		t.Errorf("UserID = %q, want axel", p.UserID)
	}
	if !s.FirstConnected() {
		t.Error("first successful auth must latch first_connected_at")
	}
}

func TestAuthenticatorRejects(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t)
	a := NewAuthenticator(s, "axel", nil)

	// No header.
	r := httptest.NewRequest("GET", "/", nil)
	if _, err := a.Verify(r); err == nil {
		t.Fatal("missing bearer must fail")
	}

	// Wrong bearer — including the RAW ROOT: only the derivation
	// authenticates, so a leaked probe exchange (which involves the
	// identity subkey) can never be replayed as a credential.
	for _, bad := range []string{"clankb_wrong", EncodeRoot(s.Root())} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", "Bearer "+bad)
		if _, err := a.Verify(r); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Fatalf("bearer %q: got %v, want ErrUnauthenticated", bad, err)
		}
	}
	if s.FirstConnected() {
		t.Error("failed auths must not latch first_connected_at")
	}
}

func TestAuthenticatorRejectsOldBearerAfterRotate(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t)
	a := NewAuthenticator(s, "axel", nil)
	oldBearer, err := BearerString(s.Root())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Rotate(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+oldBearer)
	if _, err := a.Verify(r); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("rotate must revoke old bearers, got %v", err)
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
		Proof string `json:"proof"`
		Name  string `json:"name"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}

	// Verify the proof the way the phone does: HMAC over the nonce
	// with the identity subkey.
	idKey, err := identityKey(s.Root())
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, idKey)
	mac.Write(nonce)
	if want := hex.EncodeToString(mac.Sum(nil)); body.Proof != want {
		t.Fatalf("proof = %s, want %s", body.Proof, want)
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
