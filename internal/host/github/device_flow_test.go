package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeGitHub is an httptest.Server that simulates the github.com
// auth endpoints and api.github.com /user. The auth flow shape is
// driven by deviceCodeFn / accessTokenFn / userFn so each test can
// script the state machine without touching real network.
type fakeGitHub struct {
	t              *testing.T
	authSrv        *httptest.Server
	apiSrv         *httptest.Server
	deviceCodeReqs atomic.Int64
	tokenReqs      atomic.Int64
	userReqs       atomic.Int64

	mu             sync.Mutex
	clientID       string
	scope          string
	deviceCode     string
	expiresIn      int
	interval       int
	tokenSequence  []tokenStep // consumed left-to-right per token poll
	userLogin      string
	userID         int64
	gotAccessToken string
	// userAgents holds the UA header seen on every inbound request,
	// in arrival order. Lets one test assert the manager's UA is sent
	// across both the auth and api endpoints.
	userAgents []string

	// userFetchBlock, when non-nil, makes /user wait for the channel
	// to close (or for r.Context() to cancel) before responding. Lets
	// a test race CancelConnect against an in-flight user fetch.
	userFetchBlock chan struct{}
}

// tokenStep is what fakeGitHub returns for one call to /login/oauth/access_token.
// status field maps to the body GitHub would actually send back.
type tokenStep struct {
	// One of: "pending", "slow_down", "denied", "expired", "success", "error".
	kind string
	// Only used when kind=="success" or kind=="error".
	body any
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	fg := &fakeGitHub{
		t:          t,
		deviceCode: "device-code-abc",
		expiresIn:  900,
		interval:   1, // make tests fast — minimum interval the helper allows
		userLogin:  "axelengstrom",
		userID:     12345,
	}
	fg.authSrv = httptest.NewServer(http.HandlerFunc(fg.handleAuth))
	fg.apiSrv = httptest.NewServer(http.HandlerFunc(fg.handleAPI))
	t.Cleanup(func() {
		fg.authSrv.Close()
		fg.apiSrv.Close()
	})
	return fg
}

func (fg *fakeGitHub) handleAuth(w http.ResponseWriter, r *http.Request) {
	fg.mu.Lock()
	fg.userAgents = append(fg.userAgents, r.Header.Get("User-Agent"))
	fg.mu.Unlock()
	switch r.URL.Path {
	case "/login/device/code":
		fg.deviceCodeReqs.Add(1)
		_ = r.ParseForm()
		fg.mu.Lock()
		fg.clientID = r.PostForm.Get("client_id")
		fg.scope = r.PostForm.Get("scope")
		dc := fg.deviceCode
		expires := fg.expiresIn
		interval := fg.interval
		fg.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"device_code":               dc,
			"user_code":                 "ABCD-1234",
			"verification_uri":          "https://github.com/login/device",
			"verification_uri_complete": "https://github.com/login/device?user_code=ABCD-1234",
			"expires_in":                expires,
			"interval":                  interval,
		})
	case "/login/oauth/access_token":
		fg.tokenReqs.Add(1)
		fg.mu.Lock()
		var step tokenStep
		if len(fg.tokenSequence) == 0 {
			fg.mu.Unlock()
			http.Error(w, "test bug: no scripted token step", http.StatusInternalServerError)
			return
		}
		step = fg.tokenSequence[0]
		fg.tokenSequence = fg.tokenSequence[1:]
		fg.mu.Unlock()

		switch step.kind {
		case "pending":
			writeJSON(w, http.StatusOK, map[string]string{"error": "authorization_pending"})
		case "slow_down":
			writeJSON(w, http.StatusOK, map[string]string{"error": "slow_down"})
		case "denied":
			writeJSON(w, http.StatusOK, map[string]string{"error": "access_denied"})
		case "expired":
			writeJSON(w, http.StatusOK, map[string]string{"error": "expired_token"})
		case "error":
			writeJSON(w, http.StatusOK, step.body)
		case "success":
			fg.mu.Lock()
			if body, ok := step.body.(map[string]any); ok {
				if tok, ok := body["access_token"].(string); ok {
					fg.gotAccessToken = tok
				}
			}
			fg.mu.Unlock()
			writeJSON(w, http.StatusOK, step.body)
		default:
			http.Error(w, "test bug: unknown step "+step.kind, http.StatusInternalServerError)
		}
	default:
		http.NotFound(w, r)
	}
}

func (fg *fakeGitHub) handleAPI(w http.ResponseWriter, r *http.Request) {
	fg.mu.Lock()
	fg.userAgents = append(fg.userAgents, r.Header.Get("User-Agent"))
	fg.mu.Unlock()
	switch r.URL.Path {
	case "/user":
		fg.userReqs.Add(1)
		fg.mu.Lock()
		login := fg.userLogin
		id := fg.userID
		block := fg.userFetchBlock
		fg.mu.Unlock()
		if block != nil {
			select {
			case <-r.Context().Done():
				// client canceled — let the connection drop; go-github
				// will surface context.Canceled to the caller.
				return
			case <-block:
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"login": login, "id": id})
	default:
		http.NotFound(w, r)
	}
}

func (fg *fakeGitHub) scriptTokenSteps(steps ...tokenStep) {
	fg.mu.Lock()
	fg.tokenSequence = steps
	fg.mu.Unlock()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// newTestManager builds a Manager pointed at the fake GitHub, with a
// short polling-safe interval. Tests that need slow_down or expired
// drive the state machine via fg.scriptTokenSteps.
func newTestManager(t *testing.T, fg *fakeGitHub) *Manager {
	t.Helper()
	m := NewManager(t.TempDir(), "Ov23li78UDBwea5WvI5v")
	m.SetAuthBaseURL(fg.authSrv.URL)
	m.SetAPIBaseURL(fg.apiSrv.URL)
	// Tests don't have clock-skew concerns; remove the safety margin
	// so polls fire at GitHub's returned interval (1s).
	m.SetPollSafetyMargin(0)
	return m
}

func TestStartConnect_NotConfigured(t *testing.T) {
	t.Parallel()
	m := NewManager(t.TempDir(), "")
	_, err := m.StartConnect(context.Background())
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

func TestDeviceFlow_PendingThenSuccess(t *testing.T) {
	t.Parallel()
	fg := newFakeGitHub(t)
	m := newTestManager(t, fg)

	fg.scriptTokenSteps(
		tokenStep{kind: "pending"},
		tokenStep{kind: "pending"},
		tokenStep{kind: "success", body: map[string]any{
			"access_token": "gho_abc123",
			"scope":        "repo,read:user",
			"token_type":   "bearer",
		}},
	)

	start, err := m.StartConnect(context.Background())
	if err != nil {
		t.Fatalf("StartConnect: %v", err)
	}
	if start.FlowID == "" {
		t.Fatal("FlowID should not be empty")
	}
	if start.UserCode != "ABCD-1234" {
		t.Errorf("UserCode = %q, want ABCD-1234", start.UserCode)
	}
	if !strings.HasSuffix(start.VerificationURIComplete, "user_code=ABCD-1234") {
		t.Errorf("VerificationURIComplete = %q, want code suffix", start.VerificationURIComplete)
	}

	status := waitForState(t, m, start.FlowID, FlowSuccess, 5*time.Second)
	if status.GitHubLogin != "axelengstrom" {
		t.Errorf("GitHubLogin = %q, want axelengstrom", status.GitHubLogin)
	}

	// Verify the credential landed in the store.
	creds, err := m.Store().Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if creds.AccessToken != "gho_abc123" {
		t.Errorf("AccessToken = %q, want gho_abc123", creds.AccessToken)
	}
	if creds.GitHubLogin != "axelengstrom" || creds.GitHubUserID != 12345 {
		t.Errorf("user info not captured: login=%q id=%d", creds.GitHubLogin, creds.GitHubUserID)
	}
	if len(creds.Scopes) != 2 || creds.Scopes[0] != "repo" || creds.Scopes[1] != "read:user" {
		t.Errorf("Scopes = %v, want [repo read:user]", creds.Scopes)
	}
	if creds.InstalledAt.IsZero() {
		t.Error("InstalledAt should be set on success")
	}

	// Verify scope went out on the device-code request.
	fg.mu.Lock()
	wantClient := "Ov23li78UDBwea5WvI5v"
	if fg.clientID != wantClient {
		t.Errorf("clientID sent to GitHub = %q, want %q", fg.clientID, wantClient)
	}
	wantScope := strings.Join(requestedScopes(), " ")
	if fg.scope != wantScope {
		t.Errorf("scope sent to GitHub = %q, want %q", fg.scope, wantScope)
	}
	uas := append([]string(nil), fg.userAgents...)
	fg.mu.Unlock()

	// Every outbound request must carry our User-Agent — GitHub
	// requires it on most endpoints, and the migration to oauth2 +
	// go-github means we rely on a shared RoundTripper to inject it.
	if len(uas) == 0 {
		t.Fatal("expected at least one inbound request, got 0")
	}
	for i, ua := range uas {
		if ua != userAgent {
			t.Errorf("request[%d] User-Agent = %q, want %q", i, ua, userAgent)
		}
	}
}

func TestDeviceFlow_Denied(t *testing.T) {
	t.Parallel()
	fg := newFakeGitHub(t)
	m := newTestManager(t, fg)
	fg.scriptTokenSteps(tokenStep{kind: "denied"})

	start, err := m.StartConnect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	status := waitForState(t, m, start.FlowID, FlowDenied, 5*time.Second)
	if status.Error == "" {
		t.Error("Error message should be populated on denial")
	}
	// No credential should be written.
	if m.Store().IsConnected() {
		t.Error("Store should not be connected after denial")
	}
}

func TestDeviceFlow_ExpiredFromGitHub(t *testing.T) {
	t.Parallel()
	fg := newFakeGitHub(t)
	m := newTestManager(t, fg)
	fg.scriptTokenSteps(tokenStep{kind: "expired"})

	start, err := m.StartConnect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	waitForState(t, m, start.FlowID, FlowExpired, 5*time.Second)
}

func TestDeviceFlow_SlowDownThenSuccess(t *testing.T) {
	t.Parallel()
	fg := newFakeGitHub(t)
	m := newTestManager(t, fg)
	fg.scriptTokenSteps(
		tokenStep{kind: "slow_down"},
		tokenStep{kind: "pending"},
		tokenStep{kind: "success", body: map[string]any{"access_token": "gho_slow"}},
	)

	start, err := m.StartConnect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// slow_down adds 5s — make the timeout generous to absorb it.
	waitForState(t, m, start.FlowID, FlowSuccess, 15*time.Second)
}

func TestDeviceFlow_Cancel(t *testing.T) {
	t.Parallel()
	fg := newFakeGitHub(t)
	m := newTestManager(t, fg)
	// Script enough pending steps that the flow stays pending long
	// enough for us to cancel it.
	fg.scriptTokenSteps(
		tokenStep{kind: "pending"},
		tokenStep{kind: "pending"},
		tokenStep{kind: "pending"},
	)

	start, err := m.StartConnect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Cancel while pending — should transition to canceled.
	if err := m.CancelConnect(context.Background(), start.FlowID); err != nil {
		t.Fatalf("CancelConnect: %v", err)
	}
	status := waitForState(t, m, start.FlowID, FlowCanceled, 5*time.Second)
	if status.State != FlowCanceled {
		t.Errorf("State = %q, want canceled", status.State)
	}
}

func TestDeviceFlow_SecondStartCancelsFirst(t *testing.T) {
	t.Parallel()
	fg := newFakeGitHub(t)
	m := newTestManager(t, fg)
	fg.scriptTokenSteps(
		tokenStep{kind: "pending"},
		tokenStep{kind: "pending"},
		tokenStep{kind: "success", body: map[string]any{"access_token": "gho_second"}},
	)

	first, err := m.StartConnect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Immediately start a second flow — should replace the first.
	second, err := m.StartConnect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.FlowID == second.FlowID {
		t.Fatal("second start should yield a distinct flow id")
	}
	// First flow's ID should no longer be queryable.
	if _, err := m.ConnectStatus(context.Background(), first.FlowID); !errors.Is(err, ErrUnknownFlow) {
		t.Errorf("first flow status after replacement: err = %v, want ErrUnknownFlow", err)
	}
	// Second flow should reach success.
	waitForState(t, m, second.FlowID, FlowSuccess, 10*time.Second)
}

// TestDeviceFlow_CancelDuringUserFetch_PreservesCanceled drives the
// race between CancelConnect and the post-token-exchange user fetch:
// once the goroutine clears the oauth2 polling step and enters
// getAuthenticatedUser, CancelConnect should settle the flow as
// FlowCanceled and the goroutine's late "user fetch failed"
// transition must NOT overwrite that with FlowError.
func TestDeviceFlow_CancelDuringUserFetch_PreservesCanceled(t *testing.T) {
	t.Parallel()
	fg := newFakeGitHub(t)
	// Hold /user until the test closes the channel (it doesn't). The
	// handler will release when r.Context() cancels.
	fg.mu.Lock()
	fg.userFetchBlock = make(chan struct{})
	fg.mu.Unlock()

	m := newTestManager(t, fg)
	fg.scriptTokenSteps(tokenStep{kind: "success", body: map[string]any{
		"access_token": "gho_will_be_dropped",
		"scope":        "repo,read:user",
		"token_type":   "bearer",
	}})

	start, err := m.StartConnect(context.Background())
	if err != nil {
		t.Fatalf("StartConnect: %v", err)
	}

	// Wait until the goroutine has cleared token exchange and is
	// blocked inside /user. Without this, CancelConnect could race
	// the goroutine into the token-exchange path instead.
	deadline := time.Now().Add(5 * time.Second)
	for fg.userReqs.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for /user to be hit")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := m.CancelConnect(context.Background(), start.FlowID); err != nil {
		t.Fatalf("CancelConnect: %v", err)
	}

	// CancelConnect synchronously sets FlowCanceled. Without the fix
	// in transition(), the goroutine wakes a moment later (when its
	// HTTP request cancels), reads err != nil, and overwrites
	// FlowCanceled with FlowError. The state we observe after a brief
	// settle is what discriminates the two implementations.
	//
	// Poll until userReqs stops climbing AND state has stabilized for
	// 200ms — that's a robust signal that runDeviceFlow has returned.
	stableSince := time.Now()
	lastState := FlowPending
	deadline = time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("state didn't stabilize within 5s (last=%q)", lastState)
		}
		s, err := m.ConnectStatus(context.Background(), start.FlowID)
		if err != nil {
			t.Fatalf("ConnectStatus: %v", err)
		}
		if s.State != lastState {
			lastState = s.State
			stableSince = time.Now()
		} else if time.Since(stableSince) > 200*time.Millisecond && lastState != FlowPending {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if lastState != FlowCanceled {
		t.Errorf("State = %q, want %q — transition() overwrote a settled FlowCanceled", lastState, FlowCanceled)
	}
	if m.Store().IsConnected() {
		t.Error("Store should not be connected after cancel-during-fetch")
	}
}

func TestConnectStatus_UnknownFlow(t *testing.T) {
	t.Parallel()
	fg := newFakeGitHub(t)
	m := newTestManager(t, fg)
	if _, err := m.ConnectStatus(context.Background(), "nonexistent"); !errors.Is(err, ErrUnknownFlow) {
		t.Errorf("err = %v, want ErrUnknownFlow", err)
	}
}

func TestParseScopes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want []string
	}{
		{in: "", want: nil},
		{in: "  ", want: nil},
		{in: "repo", want: []string{"repo"}},
		{in: "repo,read:user", want: []string{"repo", "read:user"}},
		{in: "repo, read:user ,workflow", want: []string{"repo", "read:user", "workflow"}},
	}
	for _, tc := range cases {
		got := parseScopes(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("parseScopes(%q) len = %d, want %d", tc.in, len(got), len(tc.want))
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("parseScopes(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

// waitForState polls the manager until the flow reaches want or the
// deadline expires. Fails the test on timeout.
func waitForState(t *testing.T, m *Manager, flowID string, want DeviceFlowState, timeout time.Duration) DeviceFlowStatus {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		status, err := m.ConnectStatus(context.Background(), flowID)
		if err == nil && status.State == want {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatalf("flow %s never reached state %q (last status=%+v err=%v)", flowID, want, status, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
