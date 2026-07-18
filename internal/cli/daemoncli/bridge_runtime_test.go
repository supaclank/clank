package daemoncli

import (
	"bytes"
	"crypto/hmac"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acksell/clank/internal/bridge"
	"github.com/acksell/clank/pkg/blobstore"
)

// TestSetupBridgeCloudModeIsNil is the cloud guard: a TCP-mode daemon
// must get no bridge — no runtime, no bridge.json, and (via
// buildHubHandler's nil admin) no /v1/bridge routes. supaclank.com
// runs this mode; the bridge is a laptop feature.
func TestSetupBridgeCloudModeIsNil(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLANK_DIR", dir)

	br := setupBridge(ServerOptions{Listen: "tcp://127.0.0.1:0"}, nil)
	if br != nil {
		t.Fatal("cloud mode must not construct a bridge runtime")
	}
	if _, err := os.Stat(filepath.Join(dir, bridgeStateFile)); !os.IsNotExist(err) {
		t.Fatal("cloud mode must not create bridge.json")
	}
	if h := br.AdminHandler(); h != nil {
		t.Fatal("nil runtime must yield nil admin handler")
	}
	br.Start(nil) // must be a nil-safe no-op
	br.Close()
}

// TestReconcileBlob_RetriesAfterConstructorFailure pins a bug where a
// failed blobstore construction still latched blobHost to the new
// host, so every later reconcileBlob call for that same host
// early-returned without retrying — image uploads stayed permanently
// disabled until the address changed again or the daemon restarted.
func TestReconcileBlob_RetriesAfterConstructorFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLANK_DIR", dir)

	br := setupBridge(ServerOptions{}, testLogger(t))
	if br == nil {
		t.Fatal("laptop mode must construct the bridge")
	}
	t.Cleanup(br.Close)

	const host = "100.64.1.2"
	status := bridge.Status{Binds: []bridge.BindStatus{{IP: host, Reason: "tailnet"}}}

	calls := 0
	br.newBlob = func(bindAddr, advertiseHost string, signKey []byte) (*blobstore.LAN, error) {
		calls++
		if calls == 1 {
			return nil, fmt.Errorf("simulated bind failure")
		}
		return blobstore.NewLAN(bindAddr, advertiseHost, signKey)
	}

	br.reconcileBlob(status)
	if br.blob != nil {
		t.Fatal("first reconcileBlob call should have failed to construct a blobstore")
	}

	// Same host again, simulating the next Refresh poll after the
	// transient failure.
	br.reconcileBlob(status)
	if br.blob == nil {
		t.Fatal("reconcileBlob must retry construction for the same host after a prior failure")
	}
	if calls != 2 {
		t.Fatalf("newBlob calls = %d, want 2 (no retry-skip)", calls)
	}
}

func TestSetupBridgeLaptopMode(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLANK_DIR", dir)

	br := setupBridge(ServerOptions{}, testLogger(t))
	if br == nil {
		t.Fatal("laptop mode must construct the bridge")
	}
	t.Cleanup(br.Close)

	fi, err := os.Stat(filepath.Join(dir, bridgeStateFile))
	if err != nil {
		t.Fatalf("bridge.json: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("bridge.json mode = %o, want 600", perm)
	}
	if br.Images() == nil {
		t.Error("laptop mode must wire the images server")
	}
}

// TestBridgePhoneSurface drives the real listener chain the phone
// hits: unauthenticated probe proves identity, bearer-less API calls
// 401, the derived bearer reaches the inner handler and latches
// first_connected, and rotation revokes it — all over a real TCP
// bind on loopback.
func TestBridgePhoneSurface(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLANK_DIR", dir)
	t.Setenv(envBridgePort, fmt.Sprintf("%d", freeTestPort(t)))

	br := setupBridge(ServerOptions{}, testLogger(t))
	if br == nil {
		t.Fatal("setupBridge returned nil")
	}
	t.Cleanup(br.Close)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	br.Start(inner)

	base := fmt.Sprintf("http://127.0.0.1:%d", br.port)

	// Admin status: pair token present, nothing connected yet.
	status := adminStatus(t, br)
	if status.PairToken == "" || status.FirstConnected {
		t.Fatalf("fresh status = %+v; want pair token + not connected", status)
	}
	root, err := bridge.DecodeRoot(status.PairToken)
	if err != nil {
		t.Fatalf("pair token invalid: %v", err)
	}

	// Probe: identity proof verifies against the shared root, with no
	// credentials sent.
	nonce := bytes.Repeat([]byte{0xAB}, 16)
	resp, err := http.Get(base + "/bridge/ping?nonce=" + hex.EncodeToString(nonce))
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	var probe struct {
		Proof string `json:"proof"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&probe); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	wantProof, err := bridge.Proof(root, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if !hmac.Equal([]byte(probe.Proof), []byte(wantProof)) {
		t.Fatalf("probe proof mismatch: got %s want %s", probe.Proof, wantProof)
	}

	// No bearer → 401 before the inner handler.
	resp, err = http.Get(base + "/v1/repos")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bearer-less request = %d, want 401", resp.StatusCode)
	}

	// Derived bearer → inner handler, and the first-connected latch flips.
	bearer, err := bridge.BearerString(root)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", base+"/v1/repos", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("authed request = %d, want 204", resp.StatusCode)
	}
	if status := adminStatus(t, br); !status.FirstConnected {
		t.Fatal("first authenticated request must latch first_connected")
	}

	// Rotate revokes: old bearer 401s, pair token changes.
	rec := httptest.NewRecorder()
	br.AdminHandler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/bridge/rotate", nil))
	if rec.Code != 200 {
		t.Fatalf("rotate = %d", rec.Code)
	}
	var rotated bridgeStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&rotated); err != nil {
		t.Fatal(err)
	}
	if rotated.PairToken == status.PairToken || rotated.FirstConnected {
		t.Fatal("rotate must change the pair token and re-arm first_connected")
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old bearer after rotate = %d, want 401", resp.StatusCode)
	}
}

func TestBridgeTrustNetworkPersists(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLANK_DIR", dir)
	t.Setenv(envBridgePort, fmt.Sprintf("%d", freeTestPort(t)))

	br := setupBridge(ServerOptions{}, testLogger(t))
	if br == nil {
		t.Fatal("setupBridge returned nil")
	}
	t.Cleanup(br.Close)
	br.Start(http.NewServeMux())

	body := strings.NewReader(`{"fingerprint":"fp-test","label":"test net"}`)
	rec := httptest.NewRecorder()
	br.AdminHandler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/bridge/trust-network", body))
	if rec.Code != 200 {
		t.Fatalf("trust-network = %d: %s", rec.Code, rec.Body.String())
	}
	raw, err := os.ReadFile(filepath.Join(dir, bridgeStateFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "fp-test") {
		t.Fatal("consent did not persist to bridge.json")
	}

	// Empty fingerprint must be rejected.
	rec = httptest.NewRecorder()
	br.AdminHandler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/bridge/trust-network", strings.NewReader(`{"fingerprint":""}`)))
	if rec.Code != 400 {
		t.Fatalf("empty fingerprint = %d, want 400", rec.Code)
	}
}

func adminStatus(t *testing.T, br *bridgeRuntime) bridgeStatusResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	br.AdminHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/bridge/status", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out bridgeStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func freeTestPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func testLogger(t *testing.T) *log.Logger {
	t.Helper()
	return log.New(testWriter{t}, "", 0)
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", bytes.TrimRight(p, "\n"))
	return len(p), nil
}
