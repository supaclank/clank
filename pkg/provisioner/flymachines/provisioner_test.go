package flymachines

import (
	"context"
	"errors"
	"io"
	"log"
	"path/filepath"
	"testing"
	"time"

	"github.com/supaclank/clank/internal/store"
	"github.com/supaclank/clank/pkg/provisioner/hoststore"
)

// mustOpenStore opens an empty real SQLite store (repo rule: no
// mocks; store-only paths run against the actual implementation).
func mustOpenStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestNew_FailsFastOnMissingOptions pins the construction guards —
// per repo rules, no fallbacks for required config.
func TestNew_FailsFastOnMissingOptions(t *testing.T) {
	t.Parallel()
	s := mustOpenStore(t)
	base := Options{APIToken: "tok", OrgSlug: "org", Region: "arn", Image: "img"}
	cases := []struct {
		name   string
		mutate func(*Options)
	}{
		{"missing-token", func(o *Options) { o.APIToken = "" }},
		{"missing-org", func(o *Options) { o.OrgSlug = "" }},
		{"missing-region", func(o *Options) { o.Region = "" }},
		{"missing-image", func(o *Options) { o.Image = "" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			opts := base
			c.mutate(&opts)
			if _, err := New(context.Background(), opts, s, nil); err == nil {
				t.Errorf("New(%+v) returned nil error", opts)
			}
		})
	}
	if _, err := New(context.Background(), base, nil, nil); err == nil {
		t.Error("New with nil store returned nil error")
	}
	if p, err := New(context.Background(), base, s, nil); err != nil {
		t.Errorf("New with complete options: %v", err)
	} else if p.GuestPreset() != "shared-8x-4096" {
		t.Errorf("GuestPreset = %q, want default shared-8x-4096", p.GuestPreset())
	}
}

// TestDestroyHostsByUser_NoHostIsNoOp: account erasure for a user
// with no machines row is a clean no-op — never reaches the Fly API.
func TestDestroyHostsByUser_NoHostIsNoOp(t *testing.T) {
	t.Parallel()
	p := &Provisioner{store: mustOpenStore(t)}
	if err := p.DestroyHostsByUser(context.Background(), "ghost"); err != nil {
		t.Fatalf("DestroyHostsByUser with no host: %v", err)
	}
}

// TestGetHostByID_StoreOnly pins that GetHostByID is a pure store
// read building a complete HostRef — no provisioning, no Fly client
// (the Provisioner under test has none).
func TestGetHostByID_StoreOnly(t *testing.T) {
	t.Parallel()
	s := mustOpenStore(t)
	p := &Provisioner{store: s}
	ctx := context.Background()

	if _, err := p.GetHostByID(ctx, "missing"); !errors.Is(err, hoststore.ErrHostNotFound) {
		t.Fatalf("missing row: want ErrHostNotFound, got %v", err)
	}

	row := hoststore.Host{
		ID:         "01HOST",
		UserID:     "u1",
		Provider:   Provider,
		ExternalID: "clank-u-abc123def456",
		Hostname:   "flym-abc123def456",
		Status:     hoststore.HostStatusRunning,
		LastURL:    "http://[fdaa:0:1::2]:8080",
		AuthToken:  "tkn",
		AutoWake:   true,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	if err := s.UpsertHost(ctx, row); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	ref, err := p.GetHostByID(ctx, "01HOST")
	if err != nil {
		t.Fatalf("GetHostByID: %v", err)
	}
	if ref.URL != row.LastURL || ref.AuthToken != "tkn" || !ref.AutoWake || ref.Hostname != row.Hostname {
		t.Errorf("ref = %+v, want fields from row %+v", ref, row)
	}
	if ref.Transport == nil {
		t.Error("ref.Transport is nil — gateway can't reach the host")
	}
}

// TestGetHostByID_FailsOnCorruptRowMissingAuthToken: a row with no
// auth_token would build a Transport that never injects Authorization
// — better to fail fast than silently serve unauthenticated requests.
func TestGetHostByID_FailsOnCorruptRowMissingAuthToken(t *testing.T) {
	t.Parallel()
	s := mustOpenStore(t)
	p := &Provisioner{store: s}
	ctx := context.Background()

	row := hoststore.Host{
		ID:         "01HOST",
		UserID:     "u1",
		Provider:   Provider,
		ExternalID: "clank-u-abc123def456",
		Hostname:   "flym-abc123def456",
		Status:     hoststore.HostStatusRunning,
		LastURL:    "http://[fdaa:0:1::2]:8080",
		AuthToken:  "",
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	if err := s.UpsertHost(ctx, row); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	if _, err := p.GetHostByID(ctx, "01HOST"); err == nil {
		t.Error("GetHostByID with empty auth_token returned nil error")
	}
}

// TestClaimThenPersistRow_PreservesCreatedAt: the claim-first flow —
// claimHostRow writes the row up front; persistRow's later
// running-status write must keep the claim's CreatedAt (and its
// tokens must round-trip unchanged) rather than overwriting them.
func TestClaimThenPersistRow_PreservesCreatedAt(t *testing.T) {
	t.Parallel()
	s := mustOpenStore(t)
	p := &Provisioner{store: s, log: log.New(io.Discard, "", 0)}
	ctx := context.Background()

	claim, err := p.claimHostRow(ctx, "u1")
	if err != nil {
		t.Fatalf("claimHostRow: %v", err)
	}
	if claim.Status != hoststore.HostStatusProvisioning {
		t.Errorf("claim status = %q, want provisioning", claim.Status)
	}
	if claim.AuthToken == "" || claim.NotifierToken == "" {
		t.Fatal("claim minted empty tokens")
	}

	time.Sleep(time.Millisecond)
	c := &cachedHost{hostID: claim.ID, appName: claim.ExternalID, hostname: claim.Hostname, url: "http://[fdaa:0:1::2]:8080"}
	tokens := hostTokens{auth: claim.AuthToken, notifier: claim.NotifierToken}
	if err := p.persistRow(ctx, "u1", c, tokens); err != nil {
		t.Fatalf("persistRow: %v", err)
	}
	updated, err := s.GetHostByID(ctx, claim.ID)
	if err != nil {
		t.Fatalf("GetHostByID after persist: %v", err)
	}
	if !updated.CreatedAt.Equal(claim.CreatedAt) {
		t.Errorf("CreatedAt changed on persist: got %v, want %v", updated.CreatedAt, claim.CreatedAt)
	}
	if !updated.UpdatedAt.After(claim.UpdatedAt) {
		t.Errorf("UpdatedAt did not advance: got %v, want after %v", updated.UpdatedAt, claim.UpdatedAt)
	}
	if updated.Status != hoststore.HostStatusRunning {
		t.Errorf("status after persist = %q, want running", updated.Status)
	}
	if updated.AuthToken != claim.AuthToken || updated.NotifierToken != claim.NotifierToken {
		t.Error("tokens changed between claim and persist")
	}
}

// TestOpenInternalConn_Validation pins the argument guards; the
// tunnel data path itself is covered end-to-end in
// internal/host/mux/tunnel_test.go.
func TestOpenInternalConn_Validation(t *testing.T) {
	t.Parallel()
	p := &Provisioner{store: mustOpenStore(t)}
	ctx := context.Background()

	if _, err := p.OpenInternalConn(ctx, "", 8080); err == nil {
		t.Error("empty hostID accepted")
	}
	if _, err := p.OpenInternalConn(ctx, "h", 0); err == nil {
		t.Error("port 0 accepted")
	}
	if _, err := p.OpenInternalConn(ctx, "missing", 8080); !errors.Is(err, hoststore.ErrHostNotFound) {
		t.Errorf("missing row: want ErrHostNotFound, got %v", err)
	}
}
