package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func mintHS256(t *testing.T, secret []byte, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := tok.SignedString(secret)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

func reqWithBearer(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

func TestJWTHS256Verify(t *testing.T) {
	t.Parallel()
	secret := []byte("test-secret")
	a := &JWTHS256{Secret: secret}
	now := time.Now()

	token := mintHS256(t, secret, jwt.MapClaims{
		"sub": "alice",
		"exp": now.Add(time.Hour).Unix(),
		"iat": now.Unix(),
	})
	p, err := a.Verify(reqWithBearer(token))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if p.UserID != "alice" {
		t.Errorf("UserID = %q, want alice", p.UserID)
	}
	if p.Claims["sub"] != "alice" {
		t.Error("Claims missing sub")
	}
}

func TestJWTHS256RejectsTamperedSig(t *testing.T) {
	t.Parallel()
	a := &JWTHS256{Secret: []byte("right-secret")}
	bad := mintHS256(t, []byte("WRONG-secret"), jwt.MapClaims{
		"sub": "alice",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	_, err := a.Verify(reqWithBearer(bad))
	if !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("err = %v, want ErrUnauthenticated", err)
	}
}

func TestJWTHS256RejectsExpired(t *testing.T) {
	t.Parallel()
	secret := []byte("s")
	a := &JWTHS256{Secret: secret}
	tok := mintHS256(t, secret, jwt.MapClaims{
		"sub": "alice",
		"exp": time.Now().Add(-time.Hour).Unix(),
	})
	_, err := a.Verify(reqWithBearer(tok))
	if !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("err = %v, want ErrUnauthenticated", err)
	}
}

func TestJWTHS256RejectsWrongAlg(t *testing.T) {
	t.Parallel()
	// "alg":"none" token — should be rejected by WithValidMethods.
	noneToken := "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0.eyJzdWIiOiJhbGljZSJ9."
	a := &JWTHS256{Secret: []byte("s")}
	_, err := a.Verify(reqWithBearer(noneToken))
	if !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("err = %v, want ErrUnauthenticated", err)
	}
}

func TestJWTHS256MissingSub(t *testing.T) {
	t.Parallel()
	secret := []byte("s")
	a := &JWTHS256{Secret: secret}
	tok := mintHS256(t, secret, jwt.MapClaims{
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	_, err := a.Verify(reqWithBearer(tok))
	if !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("err = %v, want ErrUnauthenticated", err)
	}
}

func TestJWTHS256NoBearer(t *testing.T) {
	t.Parallel()
	a := &JWTHS256{Secret: []byte("s")}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	_, err := a.Verify(r)
	if !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("err = %v, want ErrUnauthenticated", err)
	}
}

func TestJWTHS256CustomClaimMapper(t *testing.T) {
	t.Parallel()
	secret := []byte("s")
	a := &JWTHS256{
		Secret: secret,
		ClaimMapper: func(c jwt.MapClaims) (Principal, error) {
			email, _ := c["email"].(string)
			return Principal{UserID: email}, nil
		},
	}
	tok := mintHS256(t, secret, jwt.MapClaims{
		"sub":   "internal-id-42",
		"email": "alice@example.com",
		"exp":   time.Now().Add(time.Hour).Unix(),
	})
	p, err := a.Verify(reqWithBearer(tok))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if p.UserID != "alice@example.com" {
		t.Errorf("UserID = %q, want alice@example.com", p.UserID)
	}
}
