package hostmux_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/supaclank/clank/internal/agent"
	"github.com/supaclank/clank/internal/host"
	githubpkg "github.com/supaclank/clank/internal/host/github"
	hostmux "github.com/supaclank/clank/internal/host/mux"
)

// TestGitHubConnect_StartNotConfigured asserts the 503 mapping for
// ErrNotConfigured — distinct from `github_unavailable` so the
// client UI can tell "we're missing the client_id env var" from
// "the host failed to initialize the manager at all".
func TestGitHubConnect_StartNotConfigured(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv(githubpkg.ClientIDEnv, "")

	srv := newTestServer(t)
	resp, err := http.Post(srv.URL+"/credentials/github/connect/start", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	var er struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		t.Fatal(err)
	}
	if er.Code != "github_not_configured" {
		t.Errorf("code = %q, want github_not_configured", er.Code)
	}
}

func TestGitHubConnect_StatusUnknownFlow(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv(githubpkg.ClientIDEnv, "Ov23li78UDBwea5WvI5v")

	srv := newTestServer(t)
	resp, err := http.Get(srv.URL + "/credentials/github/connect/status?flow_id=nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestGitHubConnect_FlowIDRequired(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv(githubpkg.ClientIDEnv, "Ov23li78UDBwea5WvI5v")

	srv := newTestServer(t)
	resp, err := http.Get(srv.URL + "/credentials/github/connect/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status (no flow_id) = %d, want 400", resp.StatusCode)
	}

	// Same for cancel.
	resp2, err := http.Post(srv.URL+"/credentials/github/connect/cancel", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("cancel (no flow_id) = %d, want 400", resp2.StatusCode)
	}
}

// TestGitHubConnect_EndToEnd drives the start → status → success path
// through the mux against a fake GitHub. This is the gateway-style
// integration test — exercises wire shape, manager wiring, and the
// device-flow goroutine all at once.
func TestGitHubConnect_EndToEnd(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv(githubpkg.ClientIDEnv, "Ov23li78UDBwea5WvI5v")

	fg := newFakeAuth(t)
	fg.scriptToken("success", `{"access_token":"gho_mux","scope":"repo,read:user"}`)

	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
	})
	t.Cleanup(svc.Shutdown)
	// Point the manager at the fake GitHub and remove the polling
	// safety margin so tests run quickly.
	svc.GitHub().SetAuthBaseURL(fg.authSrv.URL)
	svc.GitHub().SetAPIBaseURL(fg.apiSrv.URL)
	svc.GitHub().SetPollSafetyMargin(0)

	mux := hostmux.New(svc, nil)
	srv := httptest.NewServer(mux.Handler())
	t.Cleanup(srv.Close)

	// 1) start
	resp, err := http.Post(srv.URL+"/credentials/github/connect/start", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start status = %d body=%s", resp.StatusCode, body)
	}
	var start githubpkg.DeviceFlowStart
	if err := json.Unmarshal(body, &start); err != nil {
		t.Fatal(err)
	}
	if start.FlowID == "" || start.UserCode == "" || !strings.Contains(start.VerificationURIComplete, "user_code=") {
		t.Fatalf("start payload missing fields: %+v", start)
	}

	// 2) poll status until success. The flow goroutine ticks at the
	// fake's returned interval (1s); we poll faster so we observe
	// the terminal transition shortly after it happens.
	gotSuccess := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(srv.URL + "/credentials/github/connect/status?flow_id=" + url.QueryEscape(start.FlowID))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var st githubpkg.DeviceFlowStatus
		if err := json.Unmarshal(body, &st); err != nil {
			t.Fatalf("decode status: %v body=%s", err, body)
		}
		if st.State == githubpkg.FlowSuccess {
			gotSuccess = true
			if st.GitHubLogin != "axelengstrom" {
				t.Errorf("GitHubLogin = %q, want axelengstrom", st.GitHubLogin)
			}
			break
		}
		if st.State == githubpkg.FlowError || st.State == githubpkg.FlowDenied || st.State == githubpkg.FlowExpired {
			t.Fatalf("unexpected terminal state: %+v", st)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !gotSuccess {
		t.Fatal("flow did not reach success within 10s")
	}

	// 3) status endpoint should now report connected.
	resp, err = http.Get(srv.URL + "/credentials/github/status")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	var status githubpkg.Status
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatal(err)
	}
	if !status.Connected {
		t.Errorf("status.Connected = false, want true after success")
	}
	if status.GitHubLogin != "axelengstrom" {
		t.Errorf("status.GitHubLogin = %q, want axelengstrom", status.GitHubLogin)
	}
}

// fakeAuth is a minimal stand-in for github.com used by the mux test.
// Smaller than the package-level fakeGitHub because we don't need a
// scripted multi-step token sequence — just the smallest possible
// success path.
type fakeAuth struct {
	authSrv     *httptest.Server
	apiSrv      *httptest.Server
	tokenStatus int
	tokenBody   string
}

func newFakeAuth(t *testing.T) *fakeAuth {
	t.Helper()
	fa := &fakeAuth{}
	fa.authSrv = httptest.NewServer(http.HandlerFunc(fa.handleAuth))
	fa.apiSrv = httptest.NewServer(http.HandlerFunc(fa.handleAPI))
	t.Cleanup(func() {
		fa.authSrv.Close()
		fa.apiSrv.Close()
	})
	return fa
}

func (fa *fakeAuth) handleAuth(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/login/device/code":
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"device_code":"dc-1",
			"user_code":"WXYZ-7890",
			"verification_uri":"https://github.com/login/device",
			"verification_uri_complete":"https://github.com/login/device?user_code=WXYZ-7890",
			"expires_in":900,
			"interval":1
		}`))
	case "/login/oauth/access_token":
		w.Header().Set("Content-Type", "application/json")
		if fa.tokenStatus == 0 {
			fa.tokenStatus = http.StatusOK
		}
		w.WriteHeader(fa.tokenStatus)
		_, _ = w.Write([]byte(fa.tokenBody))
	default:
		http.NotFound(w, r)
	}
}

func (fa *fakeAuth) handleAPI(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/user":
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"login":"axelengstrom","id":12345}`))
	default:
		http.NotFound(w, r)
	}
}

func (fa *fakeAuth) scriptToken(_ string, body string) {
	// Single-shot helper. Always returns 200 with the supplied JSON.
	fa.tokenStatus = http.StatusOK
	fa.tokenBody = body
}

// Quiet unused-import linter; context is referenced in the rest of
// the mux test package even if this file doesn't use it directly.
var _ = context.Background
