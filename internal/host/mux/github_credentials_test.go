package hostmux_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
	githubpkg "github.com/acksell/clank/internal/host/github"
	hostmux "github.com/acksell/clank/internal/host/mux"
)

func TestGitHubStatus_NotConfigured(t *testing.T) {
	// No CLANK_GITHUB_OAUTH_CLIENT_ID set → host advertises
	// available:false so the UI hides the connect entry.
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv(githubpkg.ClientIDEnv, "")

	srv := newTestServer(t)
	resp, body := mustGet(t, srv.URL+"/credentials/github/status")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got githubpkg.Status
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Available {
		t.Error("Available should be false when no client_id configured")
	}
	if got.Connected {
		t.Error("Connected should be false on a fresh host")
	}
}

func TestGitHubStatus_AvailableButNotConnected(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv(githubpkg.ClientIDEnv, "Ov23li78UDBwea5WvI5v")

	srv := newTestServer(t)
	resp, body := mustGet(t, srv.URL+"/credentials/github/status")
	defer resp.Body.Close()
	var got githubpkg.Status
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Available {
		t.Error("Available should be true when client_id is configured")
	}
	if got.Connected {
		t.Error("Connected should be false on a fresh host")
	}
}

func TestGitHubStatus_ConnectedAfterWrite(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv(githubpkg.ClientIDEnv, "Ov23li78UDBwea5WvI5v")

	// Seed a credential directly via the store so we can test the
	// status endpoint without the device flow (which lands in PR 2).
	store := githubpkg.NewStore(tmpHome)
	if err := store.Write(githubpkg.Credentials{
		AccessToken: "gho_test",
		GitHubLogin: "axelengstrom",
		Scopes:      []string{"repo", "read:user"},
	}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	srv := newTestServer(t)
	resp, body := mustGet(t, srv.URL+"/credentials/github/status")
	defer resp.Body.Close()
	var got githubpkg.Status
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Available {
		t.Error("Available should be true")
	}
	if !got.Connected {
		t.Error("Connected should be true after a credential write")
	}
	if got.GitHubLogin != "axelengstrom" {
		t.Errorf("GitHubLogin = %q, want axelengstrom", got.GitHubLogin)
	}
	if len(got.Scopes) != 2 {
		t.Errorf("Scopes len = %d, want 2: %v", len(got.Scopes), got.Scopes)
	}
}

func TestGitHubDisconnect_RemovesCredentialFile(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv(githubpkg.ClientIDEnv, "Ov23li78UDBwea5WvI5v")

	store := githubpkg.NewStore(tmpHome)
	if err := store.Write(githubpkg.Credentials{AccessToken: "gho_test"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	credPath := filepath.Join(tmpHome, ".local", "share", "clank", "github.json")
	if _, err := os.Stat(credPath); err != nil {
		t.Fatalf("precondition: credential file should exist: %v", err)
	}

	srv := newTestServer(t)
	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/credentials/github", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if _, err := os.Stat(credPath); !os.IsNotExist(err) {
		t.Errorf("credential file should be gone after DELETE, stat err = %v", err)
	}
}

func TestGitHubDisconnect_Idempotent(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv(githubpkg.ClientIDEnv, "Ov23li78UDBwea5WvI5v")

	srv := newTestServer(t)
	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/credentials/github", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE on missing creds: status = %d, want 204", resp.StatusCode)
	}
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
	})
	t.Cleanup(svc.Shutdown)
	mux := hostmux.New(svc, nil)
	srv := httptest.NewServer(mux.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func mustGet(t *testing.T, url string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}
