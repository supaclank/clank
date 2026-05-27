package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acksell/clank/pkg/auth"
	"github.com/acksell/clank/pkg/preview/routestore"
	"github.com/acksell/clank/pkg/preview/routestore/memstore"
	"github.com/acksell/clank/pkg/preview/tokens"
	"github.com/acksell/clank/pkg/provisioner/hoststore"
)

// fakeHostLookup resolves notifier_token → host via an in-memory map.
// Not a mock — a degenerate real implementation of the same contract
// the pgx adapter satisfies.
type fakeHostLookup map[string]hoststore.Host

func (m fakeHostLookup) GetHostByNotifierToken(_ context.Context, token string) (hoststore.Host, error) {
	if h, ok := m[token]; ok {
		return h, nil
	}
	return hoststore.Host{}, hoststore.ErrHostNotFound
}

// previewWebhookFixture spins up a gateway with preview wired and
// returns the webhook handler under an httptest server. The store
// and host map are exposed for assertions.
type previewWebhookFixture struct {
	srv     *httptest.Server
	store   *memstore.Store
	hosts   fakeHostLookup
	gateway *Gateway
}

func newPreviewWebhookFixture(t *testing.T) *previewWebhookFixture {
	t.Helper()
	store := memstore.New(nil)
	hosts := fakeHostLookup{
		"notifier-good": {ID: "host-good", UserID: "user-good"},
	}
	g, err := NewGateway(Config{
		Provisioner:          &stubProvisioner{},
		PreviewRoutes:        store,
		PreviewHostLookup:    hosts,
		PreviewRootDomain:    "clankexample.dev",
		PreviewAuthenticator: &auth.AllowAll{UserID: "ignored-in-webhook-tests"},
	}, nil)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	h := g.PreviewWebhookHandler()
	if h == nil {
		t.Fatal("PreviewWebhookHandler returned nil despite preview wired")
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &previewWebhookFixture{srv: srv, store: store, hosts: hosts, gateway: g}
}

func (f *previewWebhookFixture) postJSON(t *testing.T, path, bearer string, body any) *http.Response {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest("POST", f.srv.URL+path, bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

func TestPreviewWebhook_Register_MintsTokenAndReturnsURL(t *testing.T) {
	t.Parallel()
	f := newPreviewWebhookFixture(t)
	resp := f.postJSON(t, "/webhooks/preview/register", "notifier-good", previewRegisterRequest{
		WorktreeID:   "wt-1",
		ServiceName:  "default",
		InternalPort: 12345,
	})
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	var out previewRegisterResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Token == "" || !strings.Contains(out.URL, out.Token) {
		t.Errorf("response: token=%q url=%q", out.Token, out.URL)
	}
	if out.Visibility != tokens.VisibilityOwnerOnly {
		t.Errorf("new tokens should default to owner_only, got %q", out.Visibility)
	}

	// Store has the row, owner_user_id == host's user_id.
	got, err := f.store.GetByToken(context.Background(), out.Token)
	if err != nil {
		t.Fatalf("get after register: %v", err)
	}
	if got.OwnerUserID != "user-good" {
		t.Errorf("owner = %q, want %q", got.OwnerUserID, "user-good")
	}
	if got.HostID != "host-good" {
		t.Errorf("host_id = %q, want %q", got.HostID, "host-good")
	}
	if got.InternalPort != 12345 {
		t.Errorf("internal_port = %d, want 12345", got.InternalPort)
	}
}

func TestPreviewWebhook_Register_IdempotentOnTriple(t *testing.T) {
	t.Parallel()
	f := newPreviewWebhookFixture(t)
	body := previewRegisterRequest{WorktreeID: "wt-x", ServiceName: "default", InternalPort: 11000}

	r1 := decodeRegister(t, f.postJSON(t, "/webhooks/preview/register", "notifier-good", body))
	body.InternalPort = 11001 // sprite restart, fresh port
	r2 := decodeRegister(t, f.postJSON(t, "/webhooks/preview/register", "notifier-good", body))

	if r2.Token != r1.Token {
		t.Errorf("sprite restart returned different token: r1=%q r2=%q", r1.Token, r2.Token)
	}
	// Port was refreshed.
	got, err := f.store.GetByToken(context.Background(), r1.Token)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.InternalPort != 11001 {
		t.Errorf("port not refreshed: %d, want 11001", got.InternalPort)
	}
}

func TestPreviewWebhook_Register_MissingBearer(t *testing.T) {
	t.Parallel()
	f := newPreviewWebhookFixture(t)
	resp := f.postJSON(t, "/webhooks/preview/register", "", previewRegisterRequest{
		WorktreeID: "w", ServiceName: "default", InternalPort: 1,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestPreviewWebhook_Register_UnknownBearer(t *testing.T) {
	t.Parallel()
	f := newPreviewWebhookFixture(t)
	resp := f.postJSON(t, "/webhooks/preview/register", "notifier-evil", previewRegisterRequest{
		WorktreeID: "w", ServiceName: "default", InternalPort: 1,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestPreviewWebhook_Register_BadInternalPort(t *testing.T) {
	t.Parallel()
	f := newPreviewWebhookFixture(t)
	for _, port := range []int{0, -1, 65536, 100000} {
		resp := f.postJSON(t, "/webhooks/preview/register", "notifier-good", previewRegisterRequest{
			WorktreeID: "w", ServiceName: "default", InternalPort: port,
		})
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("port=%d: status = %d, want 400", port, resp.StatusCode)
		}
	}
}

func TestPreviewWebhook_Register_DefaultsServiceName(t *testing.T) {
	t.Parallel()
	f := newPreviewWebhookFixture(t)
	// Empty service_name should be treated as "default" so v1 callers
	// don't have to special-case it.
	r := decodeRegister(t, f.postJSON(t, "/webhooks/preview/register", "notifier-good", previewRegisterRequest{
		WorktreeID: "wt", InternalPort: 1234,
	}))
	got, err := f.store.GetByToken(context.Background(), r.Token)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ServiceName != tokens.DefaultServiceName {
		t.Errorf("service_name = %q, want %q", got.ServiceName, tokens.DefaultServiceName)
	}
}

func TestPreviewWebhook_Revoke_Idempotent(t *testing.T) {
	t.Parallel()
	f := newPreviewWebhookFixture(t)

	// Revoke without any prior register: still 204.
	resp := f.postJSON(t, "/webhooks/preview/revoke", "notifier-good", previewRevokeRequest{
		WorktreeID: "wt-none", ServiceName: "default",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("revoke-nonexistent: status = %d, want 204", resp.StatusCode)
	}

	// Now register, revoke, revoke again — all succeed; GetByToken
	// returns ErrNotFound after the first revoke.
	r := decodeRegister(t, f.postJSON(t, "/webhooks/preview/register", "notifier-good", previewRegisterRequest{
		WorktreeID: "wt", ServiceName: "default", InternalPort: 9000,
	}))
	for i := range 2 {
		resp := f.postJSON(t, "/webhooks/preview/revoke", "notifier-good", previewRevokeRequest{
			WorktreeID: "wt", ServiceName: "default",
		})
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("revoke #%d: status = %d, want 204", i, resp.StatusCode)
		}
	}
	if _, err := f.store.GetByToken(context.Background(), r.Token); !errors.Is(err, routestore.ErrNotFound) {
		t.Errorf("post-revoke GetByToken: got %v, want ErrNotFound", err)
	}
}

func TestNewGateway_PreviewAllOrNone(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  Config
	}{
		{"routes-only", Config{Provisioner: &stubProvisioner{}, PreviewRoutes: memstore.New(nil)}},
		{"hosts-only", Config{Provisioner: &stubProvisioner{}, PreviewHostLookup: fakeHostLookup{}}},
		{"root-only", Config{Provisioner: &stubProvisioner{}, PreviewRootDomain: "x.y"}},
		{"missing-root", Config{Provisioner: &stubProvisioner{}, PreviewRoutes: memstore.New(nil), PreviewHostLookup: fakeHostLookup{}}},
		{"missing-auth", Config{
			Provisioner: &stubProvisioner{}, PreviewRoutes: memstore.New(nil),
			PreviewHostLookup: fakeHostLookup{}, PreviewRootDomain: "x.y",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewGateway(tc.cfg, nil); err == nil {
				t.Errorf("partial preview config should error, got nil")
			}
		})
	}

	// Full config validates.
	full := Config{
		Provisioner:          &stubProvisioner{},
		PreviewRoutes:        memstore.New(nil),
		PreviewHostLookup:    fakeHostLookup{},
		PreviewRootDomain:    "clankexample.dev",
		PreviewAuthenticator: &auth.AllowAll{UserID: "test"},
	}
	if _, err := NewGateway(full, nil); err != nil {
		t.Errorf("full preview config failed: %v", err)
	}
}

func decodeRegister(t *testing.T, resp *http.Response) previewRegisterResponse {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("register status=%d body=%q", resp.StatusCode, body)
	}
	var out previewRegisterResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode register: %v", err)
	}
	return out
}
