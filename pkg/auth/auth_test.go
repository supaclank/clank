package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPrincipalRoundTrip(t *testing.T) {
	t.Parallel()
	p := Principal{UserID: "alice", Claims: map[string]any{"sub": "alice"}}
	ctx := WithPrincipal(context.Background(), p)
	got, ok := PrincipalFrom(ctx)
	if !ok {
		t.Fatal("PrincipalFrom returned ok=false on populated ctx")
	}
	if got.UserID != "alice" {
		t.Errorf("UserID = %q, want alice", got.UserID)
	}
}

func TestPrincipalFromEmpty(t *testing.T) {
	t.Parallel()
	_, ok := PrincipalFrom(context.Background())
	if ok {
		t.Fatal("PrincipalFrom returned ok=true on empty ctx")
	}
}

func TestMustPrincipalPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("MustPrincipal did not panic on empty ctx")
		}
	}()
	_ = MustPrincipal(context.Background())
}

func TestExtractBearer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		header  string
		want    string
		wantErr bool
	}{
		{name: "valid", header: "Bearer abc.def.ghi", want: "abc.def.ghi"},
		{name: "no header", header: "", wantErr: true},
		{name: "missing prefix", header: "abc.def.ghi", wantErr: true},
		{name: "wrong case", header: "bearer abc", wantErr: true},
		{name: "empty token", header: "Bearer ", wantErr: true},
		{name: "spaces only", header: "Bearer    ", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			got, err := ExtractBearer(r)
			if tc.wantErr {
				if !errors.Is(err, ErrUnauthenticated) {
					t.Errorf("err = %v, want ErrUnauthenticated", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

type stubAuth struct {
	principal Principal
	err       error
}

func (s stubAuth) Verify(*http.Request) (Principal, error) {
	return s.principal, s.err
}

func TestMiddlewareSuccess(t *testing.T) {
	t.Parallel()
	want := Principal{UserID: "bob", Claims: map[string]any{"sub": "bob"}}
	var seen Principal
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = MustPrincipal(r.Context())
	})
	h := Middleware(next, stubAuth{principal: want})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if seen.UserID != "bob" {
		t.Errorf("seen.UserID = %q, want bob", seen.UserID)
	}
}

func TestMiddlewareUnauthorized(t *testing.T) {
	t.Parallel()
	called := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	})
	h := Middleware(next, stubAuth{err: ErrUnauthenticated})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if called {
		t.Error("next handler ran despite auth failure")
	}
	if !strings.Contains(rec.Header().Get("WWW-Authenticate"), "Bearer") {
		t.Error("missing or malformed WWW-Authenticate header")
	}
}
