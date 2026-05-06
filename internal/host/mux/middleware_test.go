package hostmux

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequireBearer_NoTokenIsNoop(t *testing.T) {
	t.Parallel()
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	h := requireBearer("")(next)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/anything", nil))
	if !called {
		t.Error("empty token should not gate the handler")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
}

func TestRequireBearer_RejectsMissingHeader(t *testing.T) {
	t.Parallel()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called when auth fails")
	})
	h := requireBearer("secret")(next)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/anything", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401", w.Code)
	}
	if got := w.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Errorf("WWW-Authenticate = %q; want to contain Bearer", got)
	}
}

func TestRequireBearer_RejectsWrongToken(t *testing.T) {
	t.Parallel()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called when token is wrong")
	})
	h := requireBearer("secret")(next)
	for _, hdr := range []string{
		"Bearer wrong",
		"Bearer ",
		"Bearer secret-and-then-some",
		"Basic secret", // wrong scheme
		"secret",       // missing scheme
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/anything", nil)
		req.Header.Set("Authorization", hdr)
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("Authorization=%q: status = %d; want 401", hdr, w.Code)
		}
	}
}

func TestRequireBearer_AcceptsCorrectToken(t *testing.T) {
	t.Parallel()
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	h := requireBearer("secret")(next)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/anything", nil)
	req.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(w, req)
	if !called {
		t.Error("handler should be called when token matches")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}
}
