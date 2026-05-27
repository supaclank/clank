package tokens

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSign_RejectsShortKey(t *testing.T) {
	t.Parallel()
	short := make([]byte, MinSigningKeyBytes-1)
	if _, err := Sign(short, "tok", time.Now().Add(1*time.Hour)); err == nil {
		t.Error("Sign with short key returned nil error")
	}
}

func TestSign_BoundToTokenAndExp(t *testing.T) {
	t.Parallel()
	key, err := GenerateSigningKey()
	if err != nil {
		t.Fatalf("GenerateSigningKey: %v", err)
	}
	exp := time.Now().Add(1 * time.Hour)

	sigA, err := Sign(key, "token-a", exp)
	if err != nil {
		t.Fatalf("Sign A: %v", err)
	}
	sigB, err := Sign(key, "token-b", exp)
	if err != nil {
		t.Fatalf("Sign B: %v", err)
	}
	sigAExpLater, err := Sign(key, "token-a", exp.Add(1*time.Hour))
	if err != nil {
		t.Fatalf("Sign A-later: %v", err)
	}

	if sigA == sigB {
		t.Error("different tokens produced the same signature")
	}
	if sigA == sigAExpLater {
		t.Error("different exps produced the same signature")
	}
}

func TestVerify_HappyPath(t *testing.T) {
	t.Parallel()
	key, _ := GenerateSigningKey()
	exp := time.Now().Add(1 * time.Hour)
	sig, _ := Sign(key, "tok", exp)
	if err := Verify(key, "tok", sig, exp, time.Now()); err != nil {
		t.Errorf("Verify on freshly-signed bearer: %v", err)
	}
}

func TestVerify_RejectsExpired(t *testing.T) {
	t.Parallel()
	key, _ := GenerateSigningKey()
	exp := time.Now().Add(-1 * time.Minute) // in the past
	sig, _ := Sign(key, "tok", exp)
	err := Verify(key, "tok", sig, exp, time.Now())
	if !errors.Is(err, ErrSignatureExpired) {
		t.Errorf("expected ErrSignatureExpired, got %v", err)
	}
}

func TestVerify_RejectsTamperedSig(t *testing.T) {
	t.Parallel()
	key, _ := GenerateSigningKey()
	exp := time.Now().Add(1 * time.Hour)
	sig, _ := Sign(key, "tok", exp)
	// Flip a bit.
	tampered := flipBit(sig)
	if err := Verify(key, "tok", tampered, exp, time.Now()); !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("expected ErrInvalidSignature on tampered sig, got %v", err)
	}
}

func TestVerify_RejectsTamperedExp(t *testing.T) {
	t.Parallel()
	key, _ := GenerateSigningKey()
	exp := time.Now().Add(1 * time.Hour)
	sig, _ := Sign(key, "tok", exp)
	// Extend the exp by an hour, keep the original sig — the HMAC
	// should reject because the payload changed.
	longer := exp.Add(1 * time.Hour)
	if err := Verify(key, "tok", sig, longer, time.Now()); !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("expected ErrInvalidSignature on exp extension, got %v", err)
	}
}

func TestVerify_RejectsCrossTokenSig(t *testing.T) {
	t.Parallel()
	key, _ := GenerateSigningKey()
	exp := time.Now().Add(1 * time.Hour)
	sigA, _ := Sign(key, "token-a", exp)
	// Try the sig for A on token B.
	if err := Verify(key, "token-b", sigA, exp, time.Now()); !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("expected ErrInvalidSignature on cross-token attempt, got %v", err)
	}
}

func TestVerifyFromRequest_PrefersQuery(t *testing.T) {
	t.Parallel()
	key, _ := GenerateSigningKey()
	exp := time.Now().Add(1 * time.Hour)
	sig, _ := Sign(key, "tok", exp)

	req := httptest.NewRequest("GET", "/?"+SigParam+"="+sig+"&"+ExpParam+"="+strconv.FormatInt(exp.Unix(), 10), nil)
	// Add a tampered cookie that should be ignored because the query is present.
	req.AddCookie(&http.Cookie{Name: SigParam, Value: "tampered"})
	req.AddCookie(&http.Cookie{Name: ExpParam, Value: strconv.FormatInt(exp.Unix(), 10)})
	if err := VerifyFromRequest(key, "tok", req, time.Now()); err != nil {
		t.Errorf("query-present verify: %v", err)
	}
}

func TestVerifyFromRequest_FallsBackToCookies(t *testing.T) {
	t.Parallel()
	key, _ := GenerateSigningKey()
	exp := time.Now().Add(1 * time.Hour)
	sig, _ := Sign(key, "tok", exp)

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: SigParam, Value: sig})
	req.AddCookie(&http.Cookie{Name: ExpParam, Value: strconv.FormatInt(exp.Unix(), 10)})
	if err := VerifyFromRequest(key, "tok", req, time.Now()); err != nil {
		t.Errorf("cookie verify: %v", err)
	}
}

func TestVerifyFromRequest_NoSigErrorsInvalid(t *testing.T) {
	t.Parallel()
	key, _ := GenerateSigningKey()
	req := httptest.NewRequest("GET", "/", nil)
	if err := VerifyFromRequest(key, "tok", req, time.Now()); !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("no sig: got %v, want ErrInvalidSignature", err)
	}
}

func TestSignedURL_RoundTrip(t *testing.T) {
	t.Parallel()
	key, _ := GenerateSigningKey()
	exp := time.Now().Add(1 * time.Hour)
	u, err := SignedURL(key, "tok", "clankexample.dev", "https", "", exp)
	if err != nil {
		t.Fatalf("SignedURL: %v", err)
	}
	if !strings.HasPrefix(u, "https://preview-tok.clankexample.dev/") {
		t.Errorf("unexpected scheme/host: %s", u)
	}
	if !strings.Contains(u, SigParam+"=") || !strings.Contains(u, ExpParam+"=") {
		t.Errorf("missing sig/exp in URL: %s", u)
	}

	// Local-docker shape: http + explicit port.
	u, err = SignedURL(key, "tok", "localhost", "http", "7878", exp)
	if err != nil {
		t.Fatalf("SignedURL local: %v", err)
	}
	if !strings.HasPrefix(u, "http://preview-tok.localhost:7878/") {
		t.Errorf("local URL = %q, want http://preview-tok.localhost:7878/...", u)
	}
}

func TestPortFromHost(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"localhost:7878":            "7878",
		"localhost":                 "",
		"api.example.dev":           "",
		"api.example.dev:443":       "443",
		"[::1]:7878":                "7878",
		"localhost:notanumber":      "",
	}
	for in, want := range cases {
		if got := PortFromHost(in); got != want {
			t.Errorf("PortFromHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSchemeFromRequest(t *testing.T) {
	t.Parallel()
	plain := httptest.NewRequest("GET", "/", nil)
	if got := SchemeFromRequest(plain); got != "http" {
		t.Errorf("plain request: %q, want http", got)
	}
	fwd := httptest.NewRequest("GET", "/", nil)
	fwd.Header.Set("X-Forwarded-Proto", "https")
	if got := SchemeFromRequest(fwd); got != "https" {
		t.Errorf("X-Forwarded-Proto=https: %q, want https", got)
	}
}

func TestSetSignedCookies_ParseableByVerify(t *testing.T) {
	t.Parallel()
	key, _ := GenerateSigningKey()
	exp := time.Now().Add(1 * time.Hour)
	sig, _ := Sign(key, "tok", exp)

	// Set cookies via a recorder, then replay them on a fresh request.
	w := httptest.NewRecorder()
	SetSignedCookies(w, sig, exp, true)
	cookieHeader := w.Header().Get("Set-Cookie")
	if !strings.Contains(cookieHeader, SigParam+"=") {
		t.Fatalf("expected Set-Cookie to contain %s, got %q", SigParam, w.Header().Values("Set-Cookie"))
	}

	req := httptest.NewRequest("GET", "/", nil)
	for _, sc := range w.Result().Cookies() {
		req.AddCookie(sc)
	}
	if err := VerifyFromRequest(key, "tok", req, time.Now()); err != nil {
		t.Errorf("VerifyFromRequest after Set-Cookie round-trip: %v", err)
	}
}

func flipBit(s string) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	b[0] ^= 0x01
	return string(b)
}
