package daemoncli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestBridgeSessionToken pins the two MUST invariants docs/bridge-pairing.md
// [PAIR-070]/[PAIR-081] cite but had no test backing: a session token is
// mintable only by a signed request and can't mint another token, and
// revoking its device invalidates it immediately alongside the device's
// own signed requests.
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

	// Mint: only a signed request may mint a token.
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

	// The minted token authenticates the overlay's ordinary requests.
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

	// A bearer session token cannot itself mint another token.
	mintReq, _ := http.NewRequest("POST", base+"/bridge/session-token", strings.NewReader(""))
	mintReq.Header.Set("Authorization", "Bearer "+minted.Token)
	resp, err = http.DefaultClient.Do(mintReq)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("mint via bearer = %d, want 403", resp.StatusCode)
	}

	// Revoking the device invalidates its session token immediately,
	// alongside its own signed requests.
	if _, err := br.store.RemoveDevice(pub); err != nil {
		t.Fatal(err)
	}
	req, _ = http.NewRequest("GET", base+"/v1/repos", nil)
	req.Header.Set("Authorization", "Bearer "+minted.Token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bearer request after revoke = %d, want 401", resp.StatusCode)
	}
	resp, err = http.DefaultClient.Do(signedBridgeRequest(t, priv, "GET", base+"/v1/repos", ""))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked device signed request = %d, want 401", resp.StatusCode)
	}
}
