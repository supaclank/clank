package daemoncli

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
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
	"time"

	"github.com/supaclank/clank/internal/bridge"
	"github.com/supaclank/clank/pkg/blobstore"
)

// newDeviceKey mints a phone-side keypair for tests.
func newDeviceKey(t *testing.T) (ed25519.PrivateKey, []byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv, pub
}

// signedBridgeRequest builds a request signed the way the phone signs:
// fresh nonce, current timestamp, signature over the canonical string.
func signedBridgeRequest(t *testing.T, priv ed25519.PrivateKey, method, rawURL, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, rawURL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	nonceRaw := make([]byte, 16)
	if _, err := rand.Read(nonceRaw); err != nil {
		t.Fatal(err)
	}
	nonce := hex.EncodeToString(nonceRaw)
	ts := time.Now().Unix()
	req.Header.Set(bridge.HeaderKey, bridge.EncodeKey(priv.Public().(ed25519.PublicKey)))
	req.Header.Set(bridge.HeaderTimestamp, fmt.Sprintf("%d", ts))
	req.Header.Set(bridge.HeaderNonce, nonce)
	req.Header.Set(bridge.HeaderSignature, bridge.SignRequest(priv, ts, nonce, method, req.URL.RequestURI(), []byte(body)))
	return req
}

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
// hits: unauthenticated probe proves the laptop's identity, unsigned
// API calls 401, an approved device's signed request reaches the inner
// handler (named from the registry), and revocation kills the next one
// — all over a real TCP bind on loopback.
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

	// Admin status: host key public, no devices yet.
	status := adminStatus(t, br)
	if status.HostKey == "" || len(status.Devices) != 0 {
		t.Fatalf("fresh status = %+v; want host key + empty registry", status)
	}
	hostPub, err := bridge.DecodeKey(status.HostKey)
	if err != nil {
		t.Fatalf("host key invalid: %v", err)
	}

	// Probe: the laptop signs the phone's nonce; the phone verifies
	// against the QR-learned host key. No credentials sent.
	nonce := bytes.Repeat([]byte{0xAB}, 16)
	resp, err := http.Get(base + "/bridge/ping?nonce=" + hex.EncodeToString(nonce))
	if err != nil {
		t.Fatalf("probe: %v", err)
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

	// Unsigned → 401 before the inner handler.
	resp, err = http.Get(base + "/v1/repos")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unsigned request = %d, want 401", resp.StatusCode)
	}

	// A signed request from an UNAPPROVED key also 401s.
	stranger, _ := newDeviceKey(t)
	resp, err = http.DefaultClient.Do(signedBridgeRequest(t, stranger, "GET", base+"/v1/repos", ""))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unapproved signed request = %d, want 401", resp.StatusCode)
	}

	// Approve a device; its signed request reaches the inner handler.
	priv, pub := newDeviceKey(t)
	if err := br.store.AddDevice(pub, "Pixel 8"); err != nil {
		t.Fatal(err)
	}
	resp, err = http.DefaultClient.Do(signedBridgeRequest(t, priv, "GET", base+"/v1/repos", ""))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("approved signed request = %d, want 204", resp.StatusCode)
	}
	connected := adminStatus(t, br)
	// Device attribution from the registry, timestamp set — what
	// `clank pair` waits on to clear the QR and name the phone.
	if connected.LastDevice != "Pixel 8" || connected.LastConnectedAt == nil {
		t.Fatalf("last connection = %q/%v, want registry name + timestamp", connected.LastDevice, connected.LastConnectedAt)
	}
	if len(connected.Devices) != 1 || connected.Devices[0].LastSeen == nil {
		t.Fatalf("devices after auth = %+v, want one with last_seen", connected.Devices)
	}

	// Revoke-all: the same device's next signed request 401s; the host
	// key is unchanged (returning phones still recognize the laptop).
	rec := httptest.NewRecorder()
	br.AdminHandler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/bridge/pair/revoke", strings.NewReader(`{"all":true}`)))
	if rec.Code != 200 {
		t.Fatalf("revoke all = %d", rec.Code)
	}
	var revoked bridgeStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&revoked); err != nil {
		t.Fatal(err)
	}
	if len(revoked.Devices) != 0 || revoked.HostKey != status.HostKey {
		t.Fatal("revoke-all must wipe devices and keep the host key")
	}
	resp, err = http.DefaultClient.Do(signedBridgeRequest(t, priv, "GET", base+"/v1/repos", ""))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked device request = %d, want 401", resp.StatusCode)
	}
}

// TestBridgeRevokeSingleDevice pins per-device revocation: removing
// one phone leaves the other connected.
func TestBridgeRevokeSingleDevice(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLANK_DIR", dir)
	t.Setenv(envBridgePort, fmt.Sprintf("%d", freeTestPort(t)))

	br := setupBridge(ServerOptions{}, testLogger(t))
	if br == nil {
		t.Fatal("setupBridge returned nil")
	}
	t.Cleanup(br.Close)
	br.Start(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	base := fmt.Sprintf("http://127.0.0.1:%d", br.port)

	privA, pubA := newDeviceKey(t)
	privB, pubB := newDeviceKey(t)
	if err := br.store.AddDevice(pubA, "Pixel 8"); err != nil {
		t.Fatal(err)
	}
	if err := br.store.AddDevice(pubB, "iPhone"); err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf(`{"pubkey":%q}`, bridge.EncodeKey(pubA))
	rec := httptest.NewRecorder()
	br.AdminHandler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/bridge/pair/revoke", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("revoke A = %d: %s", rec.Code, rec.Body.String())
	}

	resp, err := http.DefaultClient.Do(signedBridgeRequest(t, privA, "GET", base+"/v1/repos", ""))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked A = %d, want 401", resp.StatusCode)
	}
	resp, err = http.DefaultClient.Do(signedBridgeRequest(t, privB, "GET", base+"/v1/repos", ""))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("surviving B = %d, want 204", resp.StatusCode)
	}

	// Unknown key → 404; garbage body → 400.
	rec = httptest.NewRecorder()
	br.AdminHandler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/bridge/pair/revoke", strings.NewReader(body)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("re-revoke A = %d, want 404", rec.Code)
	}
	rec = httptest.NewRecorder()
	br.AdminHandler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/bridge/pair/revoke", strings.NewReader(`{}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty revoke = %d, want 400", rec.Code)
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
