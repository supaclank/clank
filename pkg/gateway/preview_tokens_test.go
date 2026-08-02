package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/acksell/clank/pkg/auth"
	"github.com/acksell/clank/pkg/preview/routestore"
	"github.com/acksell/clank/pkg/preview/routestore/memstore"
	"github.com/acksell/clank/pkg/preview/tokens"
)

// previewTokensFixture mounts Gateway.Handler() (which is where the
// owner-facing /v1/preview/tokens routes live) under a localAuth
// wrap, so each request runs as the configured test principal.
type previewTokensFixture struct {
	srv   *httptest.Server
	store *memstore.Store
}

func newPreviewTokensFixture(t *testing.T, userID string) *previewTokensFixture {
	t.Helper()
	store := memstore.New(nil)
	g, err := NewGateway(Config{
		Provisioner:          &stubProvisioner{},
		PreviewRoutes:        store,
		PreviewHostLookup:    fakeHostLookup{},
		PreviewRootDomain:    "clankexample.dev",
		PreviewAuthenticator: &auth.AllowAll{UserID: userID},
	}, nil)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	srv := httptest.NewServer(localAuth(g.Handler(), userID))
	t.Cleanup(srv.Close)
	return &previewTokensFixture{srv: srv, store: store}
}

// seedRoute writes a Route directly via the store with sane defaults.
func (f *previewTokensFixture) seedRoute(t *testing.T, tok, owner string) routestore.Route {
	t.Helper()
	r, err := f.store.Upsert(context.Background(), routestore.Route{
		Token:        tok,
		OwnerUserID:  owner,
		HostID:       "h-" + owner,
		WorktreeID:   "wt-" + tok,
		ServiceName:  tokens.DefaultServiceName,
		InternalPort: 19000,
		Visibility:   tokens.VisibilityOwnerOnly,
		ExpiresAt:    time.Now().Add(1 * time.Hour),
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return r
}

func (f *previewTokensFixture) do(t *testing.T, method, path string, body any) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, f.srv.URL+path, rdr)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

func TestPreviewTokens_List_OnlyOwn(t *testing.T) {
	t.Parallel()
	f := newPreviewTokensFixture(t, "alice")
	f.seedRoute(t, "tok-a1", "alice")
	f.seedRoute(t, "tok-a2", "alice")
	f.seedRoute(t, "tok-b1", "bob")

	resp := f.do(t, "GET", "/v1/preview/tokens", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var list []previewTokenView
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len(list) = %d (%v), want 2 (only alice's)", len(list), tokensOf(list))
	}
	for _, v := range list {
		if v.Token == "tok-b1" {
			t.Errorf("alice's listing leaked bob's token: %v", v)
		}
	}
}

func TestPreviewTokens_Share_OwnerCanFlip(t *testing.T) {
	t.Parallel()
	f := newPreviewTokensFixture(t, "alice")
	r := f.seedRoute(t, "share-tok", "alice")

	// Flip to public, extend TTL to 2h.
	resp := f.do(t, "POST", "/v1/preview/tokens/"+r.Token+"/share", shareRequest{
		Visibility: tokens.VisibilityPublic,
		TTL:        "2h",
	})
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	var out shareResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Visibility != tokens.VisibilityPublic {
		t.Errorf("response visibility = %q", out.Visibility)
	}
	// Verify in the store.
	got, _ := f.store.GetByToken(context.Background(), r.Token)
	if got.Visibility != tokens.VisibilityPublic {
		t.Errorf("store visibility = %q", got.Visibility)
	}
	if delta := time.Until(got.ExpiresAt); delta < 90*time.Minute || delta > 150*time.Minute {
		t.Errorf("expires_at ~2h from now; got delta %v", delta)
	}
}

func TestPreviewTokens_Share_NonOwnerGets404NotForbidden(t *testing.T) {
	t.Parallel()
	// The acting principal is alice; the token belongs to bob.
	f := newPreviewTokensFixture(t, "alice")
	r := f.seedRoute(t, "shared-by-bob", "bob")

	resp := f.do(t, "POST", "/v1/preview/tokens/"+r.Token+"/share", shareRequest{
		Visibility: tokens.VisibilityPublic,
	})
	resp.Body.Close()
	// Deliberate: 404 not 403, to avoid leaking existence of bob's token.
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("cross-tenant share: status = %d, want 404", resp.StatusCode)
	}
	// And the route was NOT mutated.
	got, err := f.store.GetByToken(context.Background(), r.Token)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Visibility != tokens.VisibilityOwnerOnly {
		t.Errorf("cross-tenant share mutated visibility to %q", got.Visibility)
	}
}

func TestPreviewTokens_Share_InvalidVisibility(t *testing.T) {
	t.Parallel()
	f := newPreviewTokensFixture(t, "alice")
	r := f.seedRoute(t, "tok", "alice")
	resp := f.do(t, "POST", "/v1/preview/tokens/"+r.Token+"/share", map[string]any{
		"visibility": "anyone",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestPreviewTokens_Share_BadTTL(t *testing.T) {
	t.Parallel()
	f := newPreviewTokensFixture(t, "alice")
	r := f.seedRoute(t, "tok", "alice")
	cases := []shareRequest{
		{Visibility: tokens.VisibilityPublic, TTL: "not-a-duration"},
		{Visibility: tokens.VisibilityPublic, TTL: "-1h"},
	}
	for _, body := range cases {
		resp := f.do(t, "POST", "/v1/preview/tokens/"+r.Token+"/share", body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("ttl=%q: status = %d, want 400", body.TTL, resp.StatusCode)
		}
	}
}

func TestPreviewTokens_Delete_ByOwner(t *testing.T) {
	t.Parallel()
	f := newPreviewTokensFixture(t, "alice")
	r := f.seedRoute(t, "delete-me", "alice")

	resp := f.do(t, "DELETE", "/v1/preview/tokens/"+r.Token, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
	if _, err := f.store.GetByToken(context.Background(), r.Token); !errors.Is(err, routestore.ErrNotFound) {
		t.Errorf("get after delete: %v", err)
	}
}

func TestPreviewTokens_Delete_NonOwnerGets404(t *testing.T) {
	t.Parallel()
	f := newPreviewTokensFixture(t, "alice")
	r := f.seedRoute(t, "bobs-tok", "bob")

	resp := f.do(t, "DELETE", "/v1/preview/tokens/"+r.Token, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	// And the route is NOT revoked.
	if _, err := f.store.GetByToken(context.Background(), r.Token); err != nil {
		t.Errorf("bob's route was revoked by alice's DELETE: %v", err)
	}
}

func TestPreviewTokens_Sign_OwnerGetsSignedURL(t *testing.T) {
	t.Parallel()
	f := newPreviewTokensFixture(t, "alice")
	r := f.seedRoute(t, "sign-me", "alice")

	resp := f.do(t, "POST", "/v1/preview/tokens/"+r.Token+"/sign", signRequest{TTL: "10m"})
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	var out signResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(out.SignedURL, tokens.SigParam+"=") {
		t.Errorf("signed URL missing %s param: %s", tokens.SigParam, out.SignedURL)
	}
	if !strings.Contains(out.SignedURL, tokens.ExpParam+"=") {
		t.Errorf("signed URL missing %s param: %s", tokens.ExpParam, out.SignedURL)
	}
	if !strings.Contains(out.SignedURL, "preview-"+r.Token) {
		t.Errorf("signed URL doesn't reference token: %s", out.SignedURL)
	}
	if delta := time.Until(out.ExpiresAt); delta < 8*time.Minute || delta > 12*time.Minute {
		t.Errorf("expires_at ~10m from now; got delta %v", delta)
	}
}

func TestPreviewTokens_Sign_CarriesWebOverlayContext(t *testing.T) {
	t.Parallel()
	f := newPreviewTokensFixture(t, "alice")
	r := f.seedRoute(t, "overlay-context", "alice")

	resp := f.do(t, "POST", "/v1/preview/tokens/"+r.Token+"/sign", signRequest{
		TTL:       "10m",
		SessionID: "session-123",
		Backend:   "claude-code",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	var out signResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	u, err := url.Parse(out.SignedURL)
	if err != nil {
		t.Fatalf("parse signed URL: %v", err)
	}
	if got := u.Query().Get(overlaySessionParam); got != "session-123" {
		t.Errorf("%s = %q, want session-123", overlaySessionParam, got)
	}
	if got := u.Query().Get(overlayBackendParam); got != "claude-code" {
		t.Errorf("%s = %q, want claude-code", overlayBackendParam, got)
	}
}

func TestPreviewTokens_Sign_NonOwnerGets404(t *testing.T) {
	t.Parallel()
	// Alice signs in; the token belongs to bob. 404 (existence-leak guard).
	f := newPreviewTokensFixture(t, "alice")
	r := f.seedRoute(t, "bobs-tok", "bob")

	resp := f.do(t, "POST", "/v1/preview/tokens/"+r.Token+"/sign", signRequest{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("cross-tenant sign: status = %d, want 404", resp.StatusCode)
	}
}

func TestPreviewTokens_Sign_BadTTL(t *testing.T) {
	t.Parallel()
	f := newPreviewTokensFixture(t, "alice")
	r := f.seedRoute(t, "tok", "alice")
	cases := []signRequest{
		{TTL: "not-a-duration"},
		{TTL: "-1h"},
		{TTL: "25h"}, // exceeds MaxSigTTL (24h)
	}
	for _, body := range cases {
		resp := f.do(t, "POST", "/v1/preview/tokens/"+r.Token+"/sign", body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("ttl=%q: status = %d, want 400", body.TTL, resp.StatusCode)
		}
	}
}

func TestPreviewTokens_Sign_RevokedRoute404(t *testing.T) {
	t.Parallel()
	f := newPreviewTokensFixture(t, "alice")
	r := f.seedRoute(t, "rev", "alice")
	if err := f.store.Revoke(context.Background(), r.Token); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	resp := f.do(t, "POST", "/v1/preview/tokens/"+r.Token+"/sign", signRequest{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 after revoke", resp.StatusCode)
	}
}

func TestPreviewTokens_Delete_Idempotent(t *testing.T) {
	t.Parallel()
	f := newPreviewTokensFixture(t, "alice")
	r := f.seedRoute(t, "tok", "alice")

	resp := f.do(t, "DELETE", "/v1/preview/tokens/"+r.Token, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("first delete: status = %d", resp.StatusCode)
	}
	// Second delete: route is now revoked → handler's GetByToken
	// returns ErrNotFound → owner-facing 404. Matches the
	// share-after-revoke semantics (you can't operate on a revoked
	// route; it's gone for all owner-facing purposes).
	resp = f.do(t, "DELETE", "/v1/preview/tokens/"+r.Token, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("second delete: status = %d, want 404 (already revoked)", resp.StatusCode)
	}
}

func tokensOf(views []previewTokenView) []string {
	out := make([]string, len(views))
	for i, v := range views {
		out[i] = v.Token
	}
	return out
}
