package gateway

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acksell/clank/pkg/preview/routestore"
	"github.com/acksell/clank/pkg/preview/routestore/memstore"
	"github.com/acksell/clank/pkg/preview/tokens"
	"github.com/acksell/clank/pkg/provisioner"
)

// probeProvisioner fails every tunnel dial (forcing the proxy's
// upstream-error path) and resolves GetHostByID to a real front-door
// test server so the probe has something to GET.
type probeProvisioner struct {
	frontDoorURL string
}

func (p *probeProvisioner) EnsureHost(context.Context, string) (provisioner.HostRef, error) {
	panic("probeProvisioner: EnsureHost")
}
func (p *probeProvisioner) SuspendHost(context.Context, string) error {
	panic("probeProvisioner: SuspendHost")
}
func (p *probeProvisioner) DestroyHost(context.Context, string) error {
	panic("probeProvisioner: DestroyHost")
}
func (p *probeProvisioner) DestroyHostsByUser(context.Context, string) error {
	panic("probeProvisioner: DestroyHostsByUser")
}
func (p *probeProvisioner) GetHostByID(_ context.Context, hostID string) (provisioner.HostRef, error) {
	return provisioner.HostRef{HostID: hostID, URL: p.frontDoorURL}, nil
}
func (p *probeProvisioner) OpenInternalConn(context.Context, string, int) (net.Conn, error) {
	return nil, errors.New("simulated tunnel dial failure")
}

// syncBuffer is a goroutine-safe log sink: the probe logs from a
// goroutine while the test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestPreviewProxy_UpstreamErrorFiresFrontDoorProbe(t *testing.T) {
	t.Parallel()

	var pingHits atomic.Int64
	frontDoor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ping" {
			t.Errorf("front door got path %q, want /ping", r.URL.Path)
		}
		pingHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(frontDoor.Close)

	const root = "clankexample.dev"
	logs := &syncBuffer{}
	store := memstore.New(nil)
	g, err := NewGateway(Config{
		Provisioner:          &probeProvisioner{frontDoorURL: frontDoor.URL},
		PreviewRoutes:        store,
		PreviewHostLookup:    fakeHostLookup{},
		PreviewRootDomain:    root,
		PreviewAuthenticator: bearerAuthenticator{},
	}, log.New(logs, "", 0))
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	srv := httptest.NewServer(g.WrapPreviewSubdomain(http.NotFoundHandler()))
	t.Cleanup(srv.Close)

	tok, err := tokens.New()
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if _, err := store.Upsert(context.Background(), routestore.Route{
		Token:        tok,
		OwnerUserID:  "owner",
		HostID:       "h-owner",
		WorktreeID:   "wt",
		ServiceName:  tokens.DefaultServiceName,
		InternalPort: 19000,
		Visibility:   tokens.VisibilityPublic,
		ExpiresAt:    time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("seed route: %v", err)
	}

	get := func() *http.Response {
		req, err := http.NewRequest("GET", srv.URL+"/", nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Host = tokens.HostPrefix + tok + "." + root
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		resp.Body.Close()
		return resp
	}

	if resp := get(); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}

	// The probe is async — poll for its log line.
	deadline := time.Now().Add(2 * time.Second)
	for {
		s := logs.String()
		if strings.Contains(s, "preview probe: host h-owner front door /ping status=200") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe log line never appeared; logs:\n%s", s)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The upstream-error line carries the route context the probe
	// pairs with.
	if s := logs.String(); !strings.Contains(s, "host h-owner port 19000 worktree wt") {
		t.Errorf("upstream error line missing route context; logs:\n%s", s)
	}

	// A second failing request inside probeMinInterval must NOT fire
	// another probe (rate limit) — give any stray goroutine a moment,
	// then assert the front door saw exactly one hit.
	if resp := get(); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("second status = %d, want 502", resp.StatusCode)
	}
	time.Sleep(150 * time.Millisecond)
	if n := pingHits.Load(); n != 1 {
		t.Errorf("front door /ping hits = %d, want 1 (probe should be rate-limited)", n)
	}
}
