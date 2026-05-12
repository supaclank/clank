package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAllowAllVerify(t *testing.T) {
	t.Parallel()
	a := &AllowAll{UserID: "local"}
	p, err := a.Verify(httptest.NewRequest(http.MethodGet, "/", nil))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if p.UserID != "local" {
		t.Errorf("UserID = %q, want local", p.UserID)
	}
}

func TestAllowAllEmptyUserID(t *testing.T) {
	t.Parallel()
	a := &AllowAll{}
	_, err := a.Verify(httptest.NewRequest(http.MethodGet, "/", nil))
	if err == nil {
		t.Error("expected error for empty UserID")
	}
}
