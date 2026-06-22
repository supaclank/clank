package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acksell/clank/internal/store"
	"github.com/acksell/clank/pkg/auth"
	"github.com/acksell/clank/pkg/notify"
	"github.com/acksell/clank/pkg/preview/routestore"
	"github.com/acksell/clank/pkg/preview/routestore/memstore"
	"github.com/acksell/clank/pkg/preview/tokens"
	"github.com/acksell/clank/pkg/provisioner/hoststore"
	clanksync "github.com/acksell/clank/pkg/sync"
	"github.com/acksell/clank/pkg/sync/storage"
)

// --- real (non-mock) test fixtures for the optional gateway surfaces ---

// notifyDeviceAdapter bridges *store.Store (store.Device) to notify.DeviceStore
// (notify.Device). Mirrors the daemon's production adapter so device deletion
// is exercised against the real SQLite devices table.
type notifyDeviceAdapter struct{ s *store.Store }

func (a notifyDeviceAdapter) UpsertDevice(ctx context.Context, d notify.Device) error {
	return a.s.UpsertDevice(ctx, store.Device{UserID: d.UserID, PushToken: d.PushToken, Platform: d.Platform})
}
func (a notifyDeviceAdapter) ListDevicesByUser(ctx context.Context, userID string) ([]notify.Device, error) {
	rows, err := a.s.ListDevicesByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]notify.Device, 0, len(rows))
	for _, r := range rows {
		out = append(out, notify.Device{UserID: r.UserID, PushToken: r.PushToken, Platform: r.Platform})
	}
	return out, nil
}
func (a notifyDeviceAdapter) DeleteDevice(ctx context.Context, userID, pushToken string) error {
	return a.s.DeleteDevice(ctx, userID, pushToken)
}
func (a notifyDeviceAdapter) DeleteDeviceByPushToken(ctx context.Context, pushToken string) error {
	return a.s.DeleteDeviceByPushToken(ctx, pushToken)
}

// stubHostLookup / stubPusher satisfy the dispatcher's other deps; neither is
// touched by account deletion (only DeleteDevicesByUser runs).
type stubHostLookup struct{}

func (stubHostLookup) GetHostByNotifierToken(context.Context, string) (hoststore.Host, error) {
	return hoststore.Host{}, hoststore.ErrHostNotFound
}

type stubPusher struct{}

func (stubPusher) Push(context.Context, []notify.Message) ([]notify.Ticket, error) { return nil, nil }

// recordingIdP is a real IdPDeleter that records the userIDs it was asked to
// delete and returns a configurable error.
type recordingIdP struct {
	mu      sync.Mutex
	deleted []string
	err     error
}

func (r *recordingIdP) DeleteUser(_ context.Context, userID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleted = append(r.deleted, userID)
	return r.err
}
func (r *recordingIdP) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.deleted...)
}

// accountGateway bundles a gateway wired with every per-user store so a test
// can assert account deletion clears each one.
type accountGateway struct {
	srv    *httptest.Server
	store  *store.Store
	mem    *storage.Memory
	prov   *stubProvisioner
	routes *memstore.Store
	idp    *recordingIdP
}

func newAccountGateway(t *testing.T, prov *stubProvisioner) *accountGateway {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mem := storage.NewMemory()
	t.Cleanup(mem.Close)
	syncSrv, err := clanksync.NewServer(clanksync.Config{Store: st, Storage: mem, PresignTTL: time.Minute}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	notifyDisp := notify.NewDispatcher(stubHostLookup{}, notifyDeviceAdapter{s: st}, stubPusher{}, nil)
	routes := memstore.New(nil)
	idp := &recordingIdP{}
	g, err := NewGateway(Config{
		Provisioner:          prov,
		Sync:                 syncSrv,
		Notify:               notifyDisp,
		PreviewRoutes:        routes,
		PreviewHostLookup:    stubHostLookup{},
		PreviewRootDomain:    "preview.example.com",
		PreviewAuthenticator: &auth.AllowAll{UserID: delUser},
		IdPDeleter:           idp,
	}, nil)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	gw := httptest.NewServer(localAuth(g.Handler(), delUser))
	t.Cleanup(gw.Close)
	return &accountGateway{srv: gw, store: st, mem: mem, prov: prov, routes: routes, idp: idp}
}

// seedAccountData populates worktree+checkpoint rows, a head-bundle row, object
// blobs, a device, and a preview route — all owned by userID.
func seedAccountData(t *testing.T, ag *accountGateway, userID, worktreeID string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if err := ag.store.InsertWorktree(ctx, clanksync.Worktree{ID: worktreeID, UserID: userID, DisplayName: "demo", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := ag.store.InsertCheckpoint(ctx, clanksync.Checkpoint{ID: worktreeID + "-ck", WorktreeID: worktreeID, HeadCommit: "h", IndexTree: "i", WorktreeTree: "w", UncommittedCommit: "u", CreatedAt: now, CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := ag.store.InsertHeadBundle(ctx, clanksync.HeadBundle{UserID: userID, TipSHA: "deadbeef", BlobKey: userID + "/heads/deadbeef.bundle"}); err != nil {
		t.Fatal(err)
	}
	if err := ag.store.UpsertDevice(ctx, store.Device{UserID: userID, PushToken: "tok-" + userID, Platform: store.DevicePlatformIOS}); err != nil {
		t.Fatal(err)
	}
	if _, err := ag.routes.Upsert(ctx, routestore.Route{Token: "rt-" + userID, OwnerUserID: userID, HostID: "h", WorktreeID: worktreeID, ServiceName: "web", InternalPort: 3000, Visibility: tokens.VisibilityOwnerOnly, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	ag.mem.Put(userID+"/checkpoints/"+worktreeID+"/"+worktreeID+"-ck/manifest.json", []byte("x"))
	ag.mem.Put(userID+"/heads/deadbeef.bundle", []byte("y"))
}

func countBlobs(ag *accountGateway, prefix string) int {
	n := 0
	for _, k := range ag.mem.Keys() {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}

// TestDeleteAccount_HappyPath: a full account delete returns 204 and clears
// every store the gateway owns for the caller.
func TestDeleteAccount_HappyPath(t *testing.T) {
	t.Parallel()
	ag := newAccountGateway(t, &stubProvisioner{})
	seedAccountData(t, ag, delUser, "wt-1")
	ctx := context.Background()

	resp := httpDelete(t, ag.srv.URL+"/v1/account")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d, want 204", resp.StatusCode)
	}
	if n := atomic.LoadInt32(&ag.prov.destroyByUserCalls); n != 1 {
		t.Fatalf("DestroyHostsByUser calls=%d, want 1", n)
	}
	if wts, _ := ag.store.ListWorktreesByUser(ctx, delUser); len(wts) != 0 {
		t.Fatalf("worktrees=%d after delete, want 0", len(wts))
	}
	if _, err := ag.store.GetHeadBundle(ctx, delUser, "deadbeef"); err == nil {
		t.Fatal("head bundle survived account delete")
	}
	if n := countBlobs(ag, delUser+"/"); n != 0 {
		t.Fatalf("blobs=%d after delete, want 0", n)
	}
	if devs, _ := ag.store.ListDevicesByUser(ctx, delUser); len(devs) != 0 {
		t.Fatalf("devices=%d after delete, want 0", len(devs))
	}
	if routes, _ := ag.routes.ListByOwner(ctx, delUser); len(routes) != 0 {
		t.Fatalf("preview routes=%d after delete, want 0", len(routes))
	}
	if got := ag.idp.calls(); len(got) != 1 || got[0] != delUser {
		t.Fatalf("IdP DeleteUser calls=%v, want [%q]", got, delUser)
	}
}

// TestDeleteAccount_TenancyIsolation: deleting the caller leaves another user's
// data fully intact across every store.
func TestDeleteAccount_TenancyIsolation(t *testing.T) {
	t.Parallel()
	ag := newAccountGateway(t, &stubProvisioner{})
	seedAccountData(t, ag, delUser, "wt-mine")
	seedAccountData(t, ag, "other", "wt-theirs")
	ctx := context.Background()

	resp := httpDelete(t, ag.srv.URL+"/v1/account")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d, want 204", resp.StatusCode)
	}
	if wts, _ := ag.store.ListWorktreesByUser(ctx, "other"); len(wts) != 1 {
		t.Fatalf("other's worktrees=%d, want 1 (deletion bled across tenants)", len(wts))
	}
	if _, err := ag.store.GetHeadBundle(ctx, "other", "deadbeef"); err != nil {
		t.Fatalf("other's head bundle deleted: %v", err)
	}
	if n := countBlobs(ag, "other/"); n != 2 {
		t.Fatalf("other's blobs=%d, want 2", n)
	}
	if devs, _ := ag.store.ListDevicesByUser(ctx, "other"); len(devs) != 1 {
		t.Fatalf("other's devices=%d, want 1", len(devs))
	}
	if routes, _ := ag.routes.ListByOwner(ctx, "other"); len(routes) != 1 {
		t.Fatalf("other's preview routes=%d, want 1", len(routes))
	}
}

// TestDeleteAccount_Idempotent: a second delete is a clean 204 no-op.
func TestDeleteAccount_Idempotent(t *testing.T) {
	t.Parallel()
	ag := newAccountGateway(t, &stubProvisioner{})
	seedAccountData(t, ag, delUser, "wt-1")

	for i := 0; i < 2; i++ {
		resp := httpDelete(t, ag.srv.URL+"/v1/account")
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("delete #%d: status %d, want 204", i+1, resp.StatusCode)
		}
	}
}

// TestDeleteAccount_HostFailureAborts: a provider teardown error returns 502
// and leaves the caller's sync rows + blobs intact for a retry.
func TestDeleteAccount_HostFailureAborts(t *testing.T) {
	t.Parallel()
	ag := newAccountGateway(t, &stubProvisioner{destroyByUserErr: errSimulated})
	seedAccountData(t, ag, delUser, "wt-keep")
	ctx := context.Background()

	resp := httpDelete(t, ag.srv.URL+"/v1/account")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502 on provider failure", resp.StatusCode)
	}
	if wts, _ := ag.store.ListWorktreesByUser(ctx, delUser); len(wts) != 1 {
		t.Fatalf("worktrees=%d after aborted delete, want 1 (must be intact)", len(wts))
	}
	if n := countBlobs(ag, delUser+"/"); n != 2 {
		t.Fatalf("blobs=%d after aborted delete, want 2 (must be intact)", n)
	}
}

// TestDeleteAccount_ForceDestroysRegardlessOfSessions documents the deliberate
// divergence from DELETE /v1/worktrees/{id} (TestDeleteWorktree_BusyMaps409):
// account deletion goes through the provisioner, which tears down compute
// unconditionally — there is no busy/409 gate, so a worktree that would block a
// per-worktree delete does not block account erasure.
func TestDeleteAccount_ForceDestroysRegardlessOfSessions(t *testing.T) {
	t.Parallel()
	ag := newAccountGateway(t, &stubProvisioner{})
	seedAccountData(t, ag, delUser, "wt-busy")

	resp := httpDelete(t, ag.srv.URL+"/v1/account")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d, want 204 (account delete must not honor a busy gate)", resp.StatusCode)
	}
	if n := atomic.LoadInt32(&ag.prov.destroyByUserCalls); n != 1 {
		t.Fatalf("DestroyHostsByUser calls=%d, want 1", n)
	}
}

// TestDeleteAccount_EmptyAccountReturns204: deleting a user with no data is a
// successful erasure (idempotent end state), not a 404.
func TestDeleteAccount_EmptyAccountReturns204(t *testing.T) {
	t.Parallel()
	ag := newAccountGateway(t, &stubProvisioner{})
	resp := httpDelete(t, ag.srv.URL+"/v1/account")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d, want 204 for an empty account", resp.StatusCode)
	}
}

// TestDeleteAccount_IdPFailureReturns500: an IdP-deletion error surfaces as a
// retryable 500 (the clank-side data is already purged).
func TestDeleteAccount_IdPFailureReturns500(t *testing.T) {
	t.Parallel()
	ag := newAccountGateway(t, &stubProvisioner{})
	ag.idp.err = errSimulated
	seedAccountData(t, ag, delUser, "wt-1")

	resp := httpDelete(t, ag.srv.URL+"/v1/account")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500 on IdP failure", resp.StatusCode)
	}
}

// TestDeleteAccount_SyncUnsetReturns503: a proxy-only gateway (no Sync) cannot
// purge data and must refuse rather than half-delete.
func TestDeleteAccount_SyncUnsetReturns503(t *testing.T) {
	t.Parallel()
	prov := &stubProvisioner{}
	g, err := NewGateway(Config{Provisioner: prov}, nil)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	gw := httptest.NewServer(localAuth(g.Handler(), delUser))
	t.Cleanup(gw.Close)

	resp := httpDelete(t, gw.URL+"/v1/account")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503 when Sync unset", resp.StatusCode)
	}
	if n := atomic.LoadInt32(&prov.destroyByUserCalls); n != 0 {
		t.Fatalf("DestroyHostsByUser calls=%d, want 0 (must refuse before any teardown)", n)
	}
}

// TestDeleteAccount_Unauthenticated: the route is behind auth — a rejected
// request 401s at the middleware and never reaches the handler.
func TestDeleteAccount_Unauthenticated(t *testing.T) {
	t.Parallel()
	prov := &stubProvisioner{}
	st, err := store.Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mem := storage.NewMemory()
	t.Cleanup(mem.Close)
	syncSrv, err := clanksync.NewServer(clanksync.Config{Store: st, Storage: mem, PresignTTL: time.Minute}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	g, err := NewGateway(Config{Provisioner: prov, Sync: syncSrv}, nil)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	gw := httptest.NewServer(auth.Middleware(g.Handler(), rejectAuth{}))
	t.Cleanup(gw.Close)

	resp := httpDelete(t, gw.URL+"/v1/account")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", resp.StatusCode)
	}
	if n := atomic.LoadInt32(&prov.destroyByUserCalls); n != 0 {
		t.Fatalf("DestroyHostsByUser calls=%d, want 0 (handler must not run unauthenticated)", n)
	}
}

// rejectAuth fails every Verify, standing in for an unauthenticated request.
type rejectAuth struct{}

func (rejectAuth) Verify(*http.Request) (auth.Principal, error) {
	return auth.Principal{}, errSimulated
}
