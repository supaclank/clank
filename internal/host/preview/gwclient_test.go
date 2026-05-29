package preview

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acksell/clank/pkg/preview/tokens"
)

// TestGWClient_DisabledIsNoop is the laptop-dev path. A nil-or-empty
// client makes both Register and Revoke return nil without touching
// the wire — so a host with no gateway can still spawn previews.
func TestGWClient_DisabledIsNoop(t *testing.T) {
	t.Parallel()
	cases := []*GWClient{
		nil,
		NewGWClient("", "tok"),
		NewGWClient("https://gateway", ""),
	}
	for i, c := range cases {
		if c.Enabled() {
			t.Errorf("case %d: Enabled() = true, want false", i)
			continue
		}
		resp, err := c.Register(context.Background(), RegisterRequest{
			WorktreeID: "w", ServiceName: "default", InternalPort: 1234,
		})
		if err != nil {
			t.Errorf("case %d: Register error: %v", i, err)
		}
		if resp.Token != "" || resp.URL != "" {
			t.Errorf("case %d: disabled Register should return zero response, got %+v", i, resp)
		}
		if err := c.Revoke(context.Background(), RevokeRequest{
			WorktreeID: "w", ServiceName: "default",
		}); err != nil {
			t.Errorf("case %d: Revoke error: %v", i, err)
		}
	}
}

// TestGWClient_RegisterMakesPOST exercises the happy path: the
// client emits the right HTTP shape (bearer + JSON body + path) and
// decodes the gateway's response correctly. Uses a real httptest
// server to verify wire-format compatibility with what the gateway
// expects on the other side.
func TestGWClient_RegisterMakesPOST(t *testing.T) {
	t.Parallel()
	var (
		gotMethod string
		gotPath   string
		gotBearer string
		gotBody   RegisterRequest
		hits      atomic.Int32
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotBearer = r.Header.Get("Authorization")
		_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(RegisterResponse{
			Token:      "tok-xyz",
			URL:        "https://preview-tok-xyz.example/",
			Visibility: tokens.VisibilityOwnerOnly,
			ExpiresAt:  time.Now().Add(24 * time.Hour),
		})
	}))
	t.Cleanup(srv.Close)

	c := NewGWClient(srv.URL, "secret-bearer")
	resp, err := c.Register(context.Background(), RegisterRequest{
		WorktreeID: "wt-1", ServiceName: "default", InternalPort: 19000,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if resp.Token != "tok-xyz" {
		t.Errorf("Token = %q", resp.Token)
	}
	if resp.URL != "https://preview-tok-xyz.example/" {
		t.Errorf("URL = %q", resp.URL)
	}
	if hits.Load() != 1 {
		t.Errorf("server hits = %d, want 1", hits.Load())
	}
	if gotMethod != "POST" {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != registerPath {
		t.Errorf("path = %q, want %s", gotPath, registerPath)
	}
	if !strings.HasPrefix(gotBearer, "Bearer ") || strings.TrimPrefix(gotBearer, "Bearer ") != "secret-bearer" {
		t.Errorf("Authorization = %q", gotBearer)
	}
	if gotBody.WorktreeID != "wt-1" || gotBody.ServiceName != "default" || gotBody.InternalPort != 19000 {
		t.Errorf("body = %+v", gotBody)
	}
}

// TestGWClient_RegisterSurfacesNon200 confirms the client treats any
// non-200 from the gateway as an error (rather than silently
// returning a zero response).
func TestGWClient_RegisterSurfacesNon200(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "missing bearer", http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	c := NewGWClient(srv.URL, "bearer")
	_, err := c.Register(context.Background(), RegisterRequest{WorktreeID: "w", ServiceName: "default", InternalPort: 1})
	if err == nil {
		t.Fatal("expected error from 401 response, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error %q doesn't mention 401", err)
	}
}

// TestGWClient_RevokeAcceptsAnySuccessCode covers the documented
// "204 is the typical success" plus any 2xx the gateway might emit.
func TestGWClient_RevokeAcceptsAnySuccessCode(t *testing.T) {
	t.Parallel()
	for _, code := range []int{200, 202, 204} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
			}))
			t.Cleanup(srv.Close)
			c := NewGWClient(srv.URL, "bearer")
			if err := c.Revoke(context.Background(), RevokeRequest{WorktreeID: "w", ServiceName: "default"}); err != nil {
				t.Errorf("Revoke for code=%d: %v", code, err)
			}
		})
	}
}
