package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStaticBearerMatch(t *testing.T) {
	t.Parallel()
	a := &StaticBearer{Token: "secret-123", UserID: "ops"}
	p, err := a.Verify(reqWithBearer("secret-123"))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if p.UserID != "ops" {
		t.Errorf("UserID = %q, want ops", p.UserID)
	}
}

func TestStaticBearerMismatch(t *testing.T) {
	t.Parallel()
	a := &StaticBearer{Token: "secret-123", UserID: "ops"}
	_, err := a.Verify(reqWithBearer("wrong"))
	if !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("err = %v, want ErrUnauthenticated", err)
	}
}

func TestStaticBearerNoHeader(t *testing.T) {
	t.Parallel()
	a := &StaticBearer{Token: "x", UserID: "y"}
	_, err := a.Verify(httptest.NewRequest(http.MethodGet, "/", nil))
	if !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("err = %v, want ErrUnauthenticated", err)
	}
}

func TestStaticBearerEmptyToken(t *testing.T) {
	t.Parallel()
	a := &StaticBearer{UserID: "y"}
	_, err := a.Verify(reqWithBearer("anything"))
	if err == nil || errors.Is(err, ErrUnauthenticated) {
		t.Errorf("err = %v, want misconfiguration error (not ErrUnauthenticated)", err)
	}
}

func TestStaticBearerEmptyUserID(t *testing.T) {
	t.Parallel()
	a := &StaticBearer{Token: "x"}
	_, err := a.Verify(reqWithBearer("x"))
	if err == nil || errors.Is(err, ErrUnauthenticated) {
		t.Errorf("err = %v, want misconfiguration error (not ErrUnauthenticated)", err)
	}
}
