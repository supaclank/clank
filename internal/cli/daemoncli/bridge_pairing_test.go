package daemoncli

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/supaclank/clank/internal/bridge"
)

// TestBridgePairingCeremony drives the full SAS handshake over the real
// listener chain: a phone commits, verifies the daemon's host-signed
// reply against hk, reveals, and derives the SAS; the laptop (unix
// admin) sees it pending and types the SAS; the phone's poll reports
// approved and its own signed request then authenticates. Nothing
// secret ever crossed the wire.
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
	hostPub := mustDecodeKey(t, adminStatus(t, br).HostKey)

	priv, pub := newDeviceKey(t)
	pubB64 := bridge.EncodeKey(pub)
	nonceP := bytes.Repeat([]byte{0x5A}, 16)
	commit := bridge.SASCommit(pub, nonceP)

	// Before a CLI polls, the window is closed — begin is refused.
	if code := beginPair(t, base, "Pixel 8", commit).status; code != http.StatusConflict {
		t.Fatalf("begin with no window = %d, want 409", code)
	}

	// The CLI leases the window (this is pairingLoop's per-tick poll).
	adminPost(t, admin, "/v1/bridge/pair/poll", "")

	// A begin with a bad commit is refused outright.
	if code := beginPair(t, base, "Pixel 8", "not-a-commit").status; code != http.StatusBadRequest {
		t.Fatalf("begin with bad commit = %d, want 400", code)
	}

	begun := beginPair(t, base, "Pixel 8", commit)
	if begun.status != http.StatusOK || begun.AttemptID == "" || begun.NonceD == "" || begun.ReplySig == "" {
		t.Fatalf("begin = %d %+v", begun.status, begun)
	}
	// The phone verifies the reply is really from the laptop (hk).
	nonceD, _ := hex.DecodeString(begun.NonceD)
	if !bridge.VerifySASReply(hostPub, begun.AttemptID, commit, nonceD, begun.ReplySig) {
		t.Fatal("daemon reply did not verify against the advertised host key")
	}

	// Pre-reveal the laptop sees nothing to type yet.
	if poll := adminPost(t, admin, "/v1/bridge/pair/poll", ""); strings.Contains(poll, "Pixel 8") {
		t.Fatalf("device promptable before reveal: %s", poll)
	}

	// A reveal that doesn't open the commit is rejected (and burns it).
	_, otherPub := newDeviceKey(t)
	if code := revealPair(t, base, begun.AttemptID, bridge.EncodeKey(otherPub), hex.EncodeToString(nonceP)); code != http.StatusBadRequest {
		t.Fatalf("mismatched reveal = %d, want 400", code)
	}
	// That burned the attempt — begin + reveal again cleanly.
	begun = beginPair(t, base, "Pixel 8", commit)
	nonceD, _ = hex.DecodeString(begun.NonceD)
	if code := revealPair(t, base, begun.AttemptID, pubB64, hex.EncodeToString(nonceP)); code != http.StatusOK {
		t.Fatalf("reveal = %d, want 200", code)
	}

	// Now the laptop sees the pending device by name, and the phone can
	// derive the SAS it displays.
	if poll := adminPost(t, admin, "/v1/bridge/pair/poll", ""); !strings.Contains(poll, "Pixel 8") {
		t.Fatalf("poll pending = %s, want the device name", poll)
	}
	sas := bridge.DeriveSAS(begun.AttemptID, commit, nonceD, pub, nonceP, hostPub)

	// Pre-approval, the phone's signed requests still 401.
	if code := doSignedGet(t, priv, base); code != http.StatusUnauthorized {
		t.Fatalf("pre-approval signed request = %d, want 401", code)
	}

	// A wrong SAS doesn't approve; the right one does.
	if body := adminPost(t, admin, "/v1/bridge/pair/complete", `{"code":"000000"}`); !strings.Contains(body, "error") {
		t.Fatalf("wrong code = %s, want error", body)
	}
	if body := adminPost(t, admin, "/v1/bridge/pair/complete", `{"code":"`+sas+`"}`); !strings.Contains(body, "Pixel 8") {
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
	if code := doSignedGet(t, priv, base); code != http.StatusNoContent {
		t.Fatalf("post-ceremony signed request = %d, want 204", code)
	}

	// And the registry shows it, named.
	status := adminStatus(t, br)
	if len(status.Devices) != 1 || status.Devices[0].Name != "Pixel 8" || status.Devices[0].PubKey != pubB64 {
		t.Fatalf("registry after ceremony = %+v", status.Devices)
	}
}

func mustDecodeKey(t *testing.T, b64 string) []byte {
	t.Helper()
	k, err := bridge.DecodeKey(b64)
	if err != nil {
		t.Fatalf("decode host key: %v", err)
	}
	return k
}

func doSignedGet(t *testing.T, priv ed25519.PrivateKey, base string) int {
	t.Helper()
	resp, err := http.DefaultClient.Do(signedBridgeRequest(t, priv, "GET", base+"/v1/repos", ""))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

type beginResult struct {
	status    int
	AttemptID string `json:"attempt_id"`
	NonceD    string `json:"nonce_d"`
	ReplySig  string `json:"reply_sig"`
}

func beginPair(t *testing.T, base, device, commit string) beginResult {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"device": device, "commit": commit})
	resp, err := http.Post(base+"/bridge/pair/begin", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer resp.Body.Close()
	out := beginResult{status: resp.StatusCode}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func revealPair(t *testing.T, base, id, devicePub, nonceP string) int {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"attempt_id": id, "device_pub": devicePub, "nonce_p": nonceP})
	resp, err := http.Post(base+"/bridge/pair/reveal", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("reveal: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
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
