package daemoncli

// End-to-end: the bridge chain in front of a REAL gateway + host, the
// exact composition runGatewayServer builds for a laptop daemon. Pins
// that the phone's flow works against production routes: probe proves
// identity pre-auth, an approved device's signed request traverses
// auth.Middleware into gw.Handler(), and the buildHubHandler admin
// surface mounts only in laptop mode.

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/bridge"
	"github.com/acksell/clank/internal/host"
	"github.com/acksell/clank/internal/host/hosttest"
	hostmux "github.com/acksell/clank/internal/host/mux"
	"github.com/acksell/clank/pkg/auth"
	"github.com/acksell/clank/pkg/gateway"
)

func newTestGateway(t *testing.T) *gateway.Gateway {
	t.Helper()
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &hosttest.StubBackendManager{},
		},
	})
	t.Cleanup(svc.Shutdown)
	hostHTTP := httptest.NewServer(hostmux.New(svc, nil).Handler())
	t.Cleanup(hostHTTP.Close)
	gw, err := gateway.NewGateway(gateway.Config{
		Provisioner: &fixedHostProvisioner{url: hostHTTP.URL, transport: http.DefaultTransport},
	}, nil)
	if err != nil {
		t.Fatalf("gateway.NewGateway: %v", err)
	}
	return gw
}

func TestBridgeOverRealGateway(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLANK_DIR", dir)
	t.Setenv(envBridgePort, fmt.Sprintf("%d", freeTestPort(t)))

	gw := newTestGateway(t)
	br := setupBridge(ServerOptions{}, testLogger(t))
	if br == nil {
		t.Fatal("setupBridge returned nil in laptop mode")
	}
	t.Cleanup(br.Close)
	br.Start(gw.Handler())

	base := fmt.Sprintf("http://127.0.0.1:%d", br.port)
	status := adminStatus(t, br)
	hostPub, err := bridge.DecodeKey(status.HostKey)
	if err != nil {
		t.Fatal(err)
	}

	// Probe first — no credentials, laptop identity verified — then the
	// real gateway route with a signed request: the phone's exact
	// connect sequence.
	nonce := vectorProbeNonce()
	resp, err := http.Get(base + "/bridge/ping?nonce=" + hex.EncodeToString(nonce))
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Sig string `json:"sig"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&probe); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	sig, err := bridge.DecodeSig(probe.Sig)
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(ed25519.PublicKey(hostPub), nonce, sig) {
		t.Fatal("probe signature did not verify against the advertised host key")
	}

	resp, err = http.Get(base + "/ping")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthed /ping = %d, want 401", resp.StatusCode)
	}

	priv, pub := newDeviceKey(t)
	if err := br.store.AddDevice(pub, "Pixel 8"); err != nil {
		t.Fatal(err)
	}
	resp, err = http.DefaultClient.Do(signedBridgeRequest(t, priv, "GET", base+"/ping", ""))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("signed /ping through real gateway = %d, want 200", resp.StatusCode)
	}
}

// TestBuildHubHandlerBridgeMounting pins the mode split on the hub
// surface itself: laptop mode serves /v1/bridge/status pre-auth via
// the admin mux; cloud mode (nil admin) lets it fall through to the
// auth-wrapped catch-all — no bridge surface exists.
func TestBuildHubHandlerBridgeMounting(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLANK_DIR", dir)
	t.Setenv(envBridgePort, fmt.Sprintf("%d", freeTestPort(t)))

	gw := newTestGateway(t)

	// Laptop: admin mounted.
	br := setupBridge(ServerOptions{}, testLogger(t))
	if br == nil {
		t.Fatal("setupBridge nil")
	}
	t.Cleanup(br.Close)
	br.Start(gw.Handler())
	laptop := buildHubHandler(gw, &auth.AllowAll{UserID: "local"}, br.AdminHandler())
	rec := httptest.NewRecorder()
	laptop.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/bridge/status", nil))
	if rec.Code != 200 {
		t.Fatalf("laptop /v1/bridge/status = %d, want 200", rec.Code)
	}

	// Cloud: nil admin — the path must NOT resolve to a bridge
	// surface. It falls to the auth-wrapped catch-all, which without
	// credentials is a 401 (and with them would just proxy-404).
	cloud := buildHubHandler(gw, &auth.JWTHS256{Secret: []byte("0123456789abcdef0123456789abcdef")}, nil)
	rec = httptest.NewRecorder()
	cloud.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/bridge/status", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("cloud /v1/bridge/status = %d, want 401 (no bridge surface)", rec.Code)
	}
}

func vectorProbeNonce() []byte {
	nonce := make([]byte, 16)
	for i := range nonce {
		nonce[i] = byte(i)
	}
	return nonce
}
