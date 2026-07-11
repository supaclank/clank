package hostmux_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
	hostmux "github.com/acksell/clank/internal/host/mux"
	"github.com/acksell/clank/pkg/gateway/previewtunnel"
	"github.com/acksell/clank/pkg/provisioner"
	transportpkg "github.com/acksell/clank/pkg/provisioner/transport"
	"github.com/acksell/clank/pkg/provisioner/tunnelclient"
)

const tunnelTestBearer = "tunnel-test-token"

// nopBackendManager satisfies agent.BackendManager for a Service the
// tunnel tests never exercise.
type nopBackendManager struct{}

func (nopBackendManager) Init(context.Context, func() ([]string, error)) error { return nil }
func (nopBackendManager) CreateBackend(context.Context, agent.BackendInvocation) (agent.SessionBackend, error) {
	return nil, errors.New("nopBackendManager: not implemented")
}
func (nopBackendManager) Shutdown() {}

// newTunnelHost stands up a real bearer-gated hostmux handler on an
// httptest server. Returns its base URL and an auth transport shaped
// like HostRef.Transport.
func newTunnelHost(t *testing.T) (baseURL string, rt http.RoundTripper) {
	t.Helper()
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: nopBackendManager{},
		},
	})
	t.Cleanup(svc.Shutdown)
	m := hostmux.New(svc, nil)
	m.SetAuthToken(tunnelTestBearer)
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)
	return srv.URL, &transportpkg.BearerInjector{Token: tunnelTestBearer}
}

// startEchoListener runs a plain TCP echo server on a loopback port.
func startEchoListener(t *testing.T) (port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func echoRoundTrip(t *testing.T, conn net.Conn, payload []byte) {
	t.Helper()
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("echo mismatch: sent %d bytes, got different content back", len(payload))
	}
}

func TestTunnelEcho(t *testing.T) {
	t.Parallel()
	baseURL, rt := newTunnelHost(t)
	port := startEchoListener(t)

	conn, err := tunnelclient.Dial(testContext(t), baseURL, rt, port)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	echoRoundTrip(t, conn, []byte("hello through the tunnel"))

	// Bundler-sized payload: must survive the websocket default 32KB
	// message read limit being disabled on both ends.
	big := make([]byte, 1<<20)
	if _, err := rand.Read(big); err != nil {
		t.Fatalf("rand: %v", err)
	}
	echoRoundTrip(t, conn, big)
}

func TestTunnelConcurrentConns(t *testing.T) {
	t.Parallel()
	baseURL, rt := newTunnelHost(t)
	port := startEchoListener(t)
	ctx := testContext(t)

	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() {
			conn, err := tunnelclient.Dial(ctx, baseURL, rt, port)
			if err != nil {
				t.Errorf("conn %d dial: %v", i, err)
				return
			}
			defer conn.Close()
			payload := bytes.Repeat([]byte{byte('a' + i)}, 64<<10)
			if _, err := conn.Write(payload); err != nil {
				t.Errorf("conn %d write: %v", i, err)
				return
			}
			got := make([]byte, len(payload))
			if _, err := io.ReadFull(conn, got); err != nil {
				t.Errorf("conn %d read: %v", i, err)
				return
			}
			if !bytes.Equal(got, payload) {
				t.Errorf("conn %d: cross-connection bleed — echo returned foreign bytes", i)
			}
		})
	}
	wg.Wait()
}

func TestTunnelWrongBearer(t *testing.T) {
	t.Parallel()
	baseURL, _ := newTunnelHost(t)
	port := startEchoListener(t)

	_, err := tunnelclient.Dial(testContext(t), baseURL, &transportpkg.BearerInjector{Token: "wrong"}, port)
	if err == nil {
		t.Fatal("dial with wrong bearer succeeded")
	}
	if errors.Is(err, tunnelclient.ErrPortUnreachable) {
		t.Fatalf("auth failure misreported as unreachable port: %v", err)
	}
}

func TestTunnelDeadPort(t *testing.T) {
	t.Parallel()
	baseURL, rt := newTunnelHost(t)

	// Reserve a port, then free it so nothing is listening.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadPort := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	_, err = tunnelclient.Dial(testContext(t), baseURL, rt, deadPort)
	if !errors.Is(err, tunnelclient.ErrPortUnreachable) {
		t.Fatalf("want ErrPortUnreachable, got: %v", err)
	}
}

func TestTunnelInvalidPortRejected(t *testing.T) {
	t.Parallel()
	baseURL, rt := newTunnelHost(t)

	// Client-side validation.
	if _, err := tunnelclient.Dial(testContext(t), baseURL, rt, 0); err == nil {
		t.Fatal("client accepted port 0")
	}

	// Server-side validation for callers bypassing the client.
	req, err := http.NewRequest(http.MethodGet, baseURL+"/tunnel/notaport", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+tunnelTestBearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("GET /tunnel/notaport: want 400, got %d", resp.StatusCode)
	}
}

// tunnelDialProvisioner is a real-Provisioner-shaped stub whose
// OpenInternalConn goes through the actual tunnelclient — the exact
// wiring a Machines-style backend uses. Other methods panic since the
// preview tunnel path doesn't call them.
type tunnelDialProvisioner struct {
	baseURL string
	rt      http.RoundTripper
}

func (p *tunnelDialProvisioner) EnsureHost(context.Context, string) (provisioner.HostRef, error) {
	panic("tunnelDialProvisioner: EnsureHost")
}
func (p *tunnelDialProvisioner) SuspendHost(context.Context, string) error {
	panic("tunnelDialProvisioner: SuspendHost")
}
func (p *tunnelDialProvisioner) DestroyHost(context.Context, string) error {
	panic("tunnelDialProvisioner: DestroyHost")
}
func (p *tunnelDialProvisioner) DestroyHostsByUser(context.Context, string) error {
	panic("tunnelDialProvisioner: DestroyHostsByUser")
}
func (p *tunnelDialProvisioner) GetHostByID(context.Context, string) (provisioner.HostRef, error) {
	panic("tunnelDialProvisioner: GetHostByID")
}
func (p *tunnelDialProvisioner) OpenInternalConn(ctx context.Context, _ string, port int) (net.Conn, error) {
	return tunnelclient.Dial(ctx, p.baseURL, p.rt, port)
}

// TestTunnelNestedWebSocket pins the previewtunnel contract: an HMR-
// style WebSocket upgraded THROUGH the tunnel (ws inside ws) must
// carry frames both ways. This is the full preview data path minus
// the gateway's subdomain routing.
func TestTunnelNestedWebSocket(t *testing.T) {
	t.Parallel()
	baseURL, rt := newTunnelHost(t)
	ctx := testContext(t)

	// A real WebSocket echo server standing in for Metro's HMR endpoint.
	wsEcho := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		nc := websocket.NetConn(r.Context(), c, websocket.MessageText)
		_, _ = io.Copy(nc, nc)
	}))
	t.Cleanup(wsEcho.Close)
	echoURL, err := url.Parse(wsEcho.URL)
	if err != nil {
		t.Fatalf("parse echo url: %v", err)
	}
	echoPort, err := strconv.Atoi(echoURL.Port())
	if err != nil {
		t.Fatalf("echo port: %v", err)
	}

	tun, err := previewtunnel.New(&tunnelDialProvisioner{baseURL: baseURL, rt: rt}, "host-1", echoPort, previewtunnel.Config{})
	if err != nil {
		t.Fatalf("previewtunnel.New: %v", err)
	}

	// The URL host is irrelevant: previewtunnel dials the configured
	// (host, port) regardless, like the gateway's reverse proxy.
	c, _, err := websocket.Dial(ctx, "ws://preview.invalid/hmr", &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: tun},
	})
	if err != nil {
		t.Fatalf("nested websocket dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	want := fmt.Sprintf("hmr-frame-%d", echoPort)
	if err := c.Write(ctx, websocket.MessageText, []byte(want)); err != nil {
		t.Fatalf("nested write: %v", err)
	}
	_, got, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("nested read: %v", err)
	}
	if string(got) != want {
		t.Fatalf("nested echo: want %q got %q", want, got)
	}
}
