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
// real listener chain: a phone begins pre-auth, the laptop (unix admin)
// sees it pending and completes with the code, the phone's poll
// delivers the root secret, and the derived bearer then authenticates.
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

	// Before a CLI polls, the window is closed — begin is refused.
	if code := beginPair(t, base, "Pixel 8").status; code != http.StatusConflict {
		t.Fatalf("begin with no window = %d, want 409", code)
	}

	// The CLI leases the window (this is pairingLoop's per-tick poll).
	adminPost(t, admin, "/v1/bridge/pair/poll", "")

	begun := beginPair(t, base, "Pixel 8")
	if begun.status != http.StatusOK || begun.AttemptID == "" || len(begun.Code) != 6 {
		t.Fatalf("begin = %d %+v", begun.status, begun)
	}

	// The laptop sees the pending device by name.
	poll := adminPost(t, admin, "/v1/bridge/pair/poll", "")
	if !strings.Contains(poll, "Pixel 8") {
		t.Fatalf("poll pending = %s, want the device name", poll)
	}

	// A wrong code doesn't approve; the right one does.
	if body := adminPost(t, admin, "/v1/bridge/pair/complete", `{"code":"000000"}`); !strings.Contains(body, "error") {
		t.Fatalf("wrong code = %s, want error", body)
	}
	if body := adminPost(t, admin, "/v1/bridge/pair/complete", `{"code":"`+begun.Code+`"}`); !strings.Contains(body, "Pixel 8") {
		t.Fatalf("complete = %s, want device", body)
	}

	// The phone's poll now delivers the root secret exactly once.
	att := attemptPoll(t, base, begun.AttemptID)
	if att.State != "approved" || att.Secret == "" {
		t.Fatalf("attempt after approval = %+v, want approved + secret", att)
	}
	if again := attemptPoll(t, base, begun.AttemptID); again.Secret != "" {
		t.Fatalf("secret delivered twice: %s", again.Secret)
	}

	// The delivered secret derives a bearer that authenticates.
	root, err := bridge.DecodeRoot(att.Secret)
	if err != nil {
		t.Fatalf("delivered secret invalid: %v", err)
	}
	bearer, err := bridge.BearerString(root)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", base+"/v1/repos", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("authed with ceremony-delivered bearer = %d, want 204", resp.StatusCode)
	}
}

type beginResult struct {
	status    int
	AttemptID string `json:"attempt_id"`
	Code      string `json:"code"`
}

func beginPair(t *testing.T, base, device string) beginResult {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"device": device})
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
