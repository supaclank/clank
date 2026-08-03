package previewtunnel

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/supaclank/clank/pkg/provisioner"
)

// dialProvisioner is a real Provisioner.OpenInternalConn that ignores
// (hostID, port) and dials a configured target address. It's the
// thinnest "real" provisioner that lets us put a Tunnel in front of
// any httptest.Server. Other Provisioner methods aren't reachable from
// the Tunnel test surface so they panic if exercised.
type dialProvisioner struct {
	target   string
	dials    atomic.Int64
	dialErr  error // returned by OpenInternalConn when set
	delay    time.Duration
	notFound bool
}

func (d *dialProvisioner) EnsureHost(context.Context, string) (provisioner.HostRef, error) {
	panic("dialProvisioner: EnsureHost not used by Tunnel")
}
func (d *dialProvisioner) SuspendHost(context.Context, string) error {
	panic("dialProvisioner: SuspendHost not used by Tunnel")
}
func (d *dialProvisioner) DestroyHost(context.Context, string) error {
	panic("dialProvisioner: DestroyHost not used by Tunnel")
}
func (d *dialProvisioner) DestroyHostsByUser(context.Context, string) error {
	panic("dialProvisioner: DestroyHostsByUser not used by Tunnel")
}
func (d *dialProvisioner) GetHostByID(context.Context, string) (provisioner.HostRef, error) {
	panic("dialProvisioner: GetHostByID not used by Tunnel")
}
func (d *dialProvisioner) OpenInternalConn(ctx context.Context, _ string, _ int) (net.Conn, error) {
	d.dials.Add(1)
	if d.delay > 0 {
		select {
		case <-time.After(d.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if d.dialErr != nil {
		return nil, d.dialErr
	}
	if d.notFound {
		return nil, errors.New("host not found")
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp", d.target)
}

func TestNew_Validation(t *testing.T) {
	t.Parallel()
	prov := &dialProvisioner{target: "127.0.0.1:1"}
	cases := []struct {
		name   string
		prov   provisioner.Provisioner
		hostID string
		port   int
	}{
		{"nil provisioner", nil, "h", 1234},
		{"empty hostID", prov, "", 1234},
		{"zero port", prov, "h", 0},
		{"negative port", prov, "h", -1},
		{"port too high", prov, "h", 70000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := New(tc.prov, tc.hostID, tc.port, Config{}); err == nil {
				t.Errorf("New(%s) = nil error, want validation failure", tc.name)
			}
		})
	}
}

func TestTunnel_RoundTripDelegatesToProvisioner(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test-Path", r.URL.Path)
		_, _ = w.Write([]byte("ok " + r.URL.Path))
	}))
	t.Cleanup(srv.Close)
	target := strings.TrimPrefix(srv.URL, "http://")

	prov := &dialProvisioner{target: target}
	tun, err := New(prov, "h-1", 12345, Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer tun.Close()

	client := &http.Client{Transport: tun}
	resp, err := client.Get("http://placeholder/hello")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "ok /hello" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Test-Path"); got != "/hello" {
		t.Errorf("X-Test-Path = %q", got)
	}
	if dials := prov.dials.Load(); dials != 1 {
		t.Errorf("dials = %d, want 1 (first request)", dials)
	}
}

func TestTunnel_NoIdleReuse_FreshDialPerRequest(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("k"))
	}))
	t.Cleanup(srv.Close)
	target := strings.TrimPrefix(srv.URL, "http://")

	prov := &dialProvisioner{target: target}
	tun, err := New(prov, "h", 1, Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer tun.Close()

	client := &http.Client{Transport: tun}
	for range 5 {
		resp, err := client.Get("http://placeholder/")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	// Idle keep-alive reuse is disabled (DisableKeepAlives): each
	// sequential request dials a FRESH tunnel rather than reusing a
	// pooled one. This is intentional — a pooled WSS tunnel can go
	// half-open (Sprites edge idle-drop / sprite pause) and reusing it
	// hangs the next request. So 5 GETs == 5 dials.
	if dials := prov.dials.Load(); dials != 5 {
		t.Errorf("dials = %d, want 5 (fresh dial per request, no idle reuse)", dials)
	}
}

func TestTunnel_ConcurrentRequestsOpenParallelTunnels(t *testing.T) {
	t.Parallel()
	// Server slow enough that serialized requests would obviously
	// stall but with a small ceiling so flake risk is bounded.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)
	target := strings.TrimPrefix(srv.URL, "http://")

	prov := &dialProvisioner{target: target}
	tun, err := New(prov, "h", 1, Config{MaxIdleConns: 16})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer tun.Close()

	client := &http.Client{Transport: tun}
	const n = 8
	var wg sync.WaitGroup
	start := time.Now()
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			resp, err := client.Get("http://placeholder/")
			if err != nil {
				t.Errorf("get: %v", err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	// 8 parallel × 50ms upstream latency: should finish in well under
	// 8 × 50ms = 400ms if tunnels open in parallel. 200ms cap gives
	// headroom for goroutine startup + dial.
	if elapsed > 200*time.Millisecond {
		t.Errorf("8 parallel requests took %s; serialization likely (want <200ms)", elapsed)
	}
	if dials := prov.dials.Load(); dials < 2 {
		t.Errorf("dials = %d under concurrency, expected pool to open >=2", dials)
	}
}

func TestTunnel_DialErrorSurfaces(t *testing.T) {
	t.Parallel()
	prov := &dialProvisioner{dialErr: errors.New("simulated dial failure")}
	tun, err := New(prov, "h", 1, Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer tun.Close()
	client := &http.Client{Transport: tun}
	_, err = client.Get("http://placeholder/")
	if err == nil {
		t.Fatal("expected error from failed dial, got nil")
	}
	if !strings.Contains(err.Error(), "simulated dial failure") {
		t.Errorf("error %q doesn't mention simulated cause", err)
	}
}

func TestTunnel_DialTimeoutEnforced(t *testing.T) {
	t.Parallel()
	prov := &dialProvisioner{
		target: "127.0.0.1:1", // ignored — we time out before dial
		delay:  500 * time.Millisecond,
	}
	tun, err := New(prov, "h", 1, Config{DialTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer tun.Close()
	client := &http.Client{Transport: tun, Timeout: 1 * time.Second}
	start := time.Now()
	_, err = client.Get("http://placeholder/")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if elapsed > 400*time.Millisecond {
		t.Errorf("DialTimeout not honored: waited %s, expected ~50ms", elapsed)
	}
}

func TestTunnel_CloseStopsService(t *testing.T) {
	t.Parallel()
	prov := &dialProvisioner{target: "127.0.0.1:1"}
	tun, err := New(prov, "h", 1, Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tun.Close()
	tun.Close() // idempotent
	req, _ := http.NewRequest("GET", "http://placeholder/", nil)
	if _, err := tun.RoundTrip(req); !errors.Is(err, ErrUninitialized) {
		t.Errorf("RoundTrip after Close: got %v, want ErrUninitialized", err)
	}
}
