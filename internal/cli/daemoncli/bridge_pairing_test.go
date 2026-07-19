package daemoncli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acksell/clank/internal/bridge"
)

// TestBridgePairingCeremony drives the full typed-code flow over the
// real listener chain: a phone begins pre-auth with its public key,
// the laptop (unix admin) sees it pending and completes with the code,
// the phone's poll reports approved — and the phone's own signed
// request then authenticates. Nothing secret ever crossed the wire.
func TestBridgePairingCeremony(t *testing.T) {
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
	admin := br.AdminHandler()

	priv, pub := newDeviceKey(t)
	pubB64 := bridge.EncodeKey(pub)

	// Before a CLI polls, the window is closed — begin is refused.
	if code := beginPair(t, base, "Pixel 8", pubB64).status; code != http.StatusConflict {
		t.Fatalf("begin with no window = %d, want 409", code)
	}

	// The CLI leases the window (this is pairingLoop's per-tick poll).
	adminPost(t, admin, "/v1/bridge/pair/poll", "")

	// A begin without a key is refused outright.
	if code := beginPair(t, base, "Pixel 8", "").status; code != http.StatusBadRequest {
		t.Fatalf("begin without key = %d, want 400", code)
	}

	begun := beginPair(t, base, "Pixel 8", pubB64)
	if begun.status != http.StatusOK || begun.AttemptID == "" || len(begun.Code) != 6 {
		t.Fatalf("begin = %d %+v", begun.status, begun)
	}

	// The laptop sees the pending device by name.
	poll := adminPost(t, admin, "/v1/bridge/pair/poll", "")
	if !strings.Contains(poll, "Pixel 8") {
		t.Fatalf("poll pending = %s, want the device name", poll)
	}

	// Pre-approval, the phone's signed requests still 401 — begin alone
	// grants nothing.
	resp, err := http.DefaultClient.Do(signedBridgeRequest(t, priv, "GET", base+"/v1/repos", ""))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("pre-approval signed request = %d, want 401", resp.StatusCode)
	}

	// A wrong code doesn't approve; the right one does.
	if body := adminPost(t, admin, "/v1/bridge/pair/complete", `{"code":"000000"}`); !strings.Contains(body, "error") {
		t.Fatalf("wrong code = %s, want error", body)
	}
	if body := adminPost(t, admin, "/v1/bridge/pair/complete", `{"code":"`+begun.Code+`"}`); !strings.Contains(body, "Pixel 8") {
		t.Fatalf("complete = %s, want device", body)
	}

	// The phone's poll reports approved — no payload.
	att := attemptPoll(t, base, begun.AttemptID)
	if att.State != "approved" {
		t.Fatalf("attempt after approval = %+v, want approved", att)
	}
	if att.Secret != "" {
		t.Fatalf("approval must carry no secret, got %q", att.Secret)
	}

	// The phone's key is now trusted: its signed request authenticates.
	resp, err = http.DefaultClient.Do(signedBridgeRequest(t, priv, "GET", base+"/v1/repos", ""))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("post-ceremony signed request = %d, want 204", resp.StatusCode)
	}

	// And the registry shows it, named.
	status := adminStatus(t, br)
	if len(status.Devices) != 1 || status.Devices[0].Name != "Pixel 8" || status.Devices[0].PubKey != pubB64 {
		t.Fatalf("registry after ceremony = %+v", status.Devices)
	}
}

// TestBridgeSessionToken pins the native overlay's credential path: a
// SIGNED request mints a short-TTL bearer, the bearer authenticates
// plain requests, a bearer cannot mint another token, and revoking the
// device kills its minted tokens.
func TestBridgeSessionToken(t *testing.T) {
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

	priv, pub := newDeviceKey(t)
	if err := br.store.AddDevice(pub, "Pixel 8"); err != nil {
		t.Fatal(err)
	}

	// Signed mint.
	resp, err := http.DefaultClient.Do(signedBridgeRequest(t, priv, "POST", base+"/bridge/session-token", ""))
	if err != nil {
		t.Fatal(err)
	}
	var minted struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&minted); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || minted.Token == "" {
		t.Fatalf("mint = %d %+v, want 200 + token", resp.StatusCode, minted)
	}

	// The bearer authenticates a plain request (the overlay's shape).
	req, _ := http.NewRequest("GET", base+"/v1/repos", nil)
	req.Header.Set("Authorization", "Bearer "+minted.Token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("bearer request = %d, want 204", resp.StatusCode)
	}

	// A bearer cannot mint: the mint route demands signature headers.
	mintReq, _ := http.NewRequest("POST", base+"/bridge/session-token", nil)
	mintReq.Header.Set("Authorization", "Bearer "+minted.Token)
	resp, err = http.DefaultClient.Do(mintReq)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bearer-authed mint = %d, want 403", resp.StatusCode)
	}

	// Revoking the device revokes its minted tokens.
	if _, err := br.store.RemoveDevice(pub); err != nil {
		t.Fatal(err)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bearer after device revoke = %d, want 401", resp.StatusCode)
	}
}

type beginResult struct {
	status    int
	AttemptID string `json:"attempt_id"`
	Code      string `json:"code"`
}

func beginPair(t *testing.T, base, device, pubB64 string) beginResult {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"device": device, "pubkey": pubB64})
	resp, err := http.Post(base+"/bridge/pair/begin", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer resp.Body.Close()
	out := beginResult{status: resp.StatusCode}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

type attemptResult struct {
	State  string `json:"state"`
	Secret string `json:"secret"`
}

func attemptPoll(t *testing.T, base, id string) attemptResult {
	t.Helper()
	resp, err := http.Get(base + "/bridge/pair/attempt?id=" + id)
	if err != nil {
		t.Fatalf("attempt poll: %v", err)
	}
	defer resp.Body.Close()
	var out attemptResult
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func adminPost(t *testing.T, admin http.Handler, path, body string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	admin.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(body)))
	return rec.Body.String()
}
