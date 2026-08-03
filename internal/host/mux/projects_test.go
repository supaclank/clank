package hostmux_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supaclank/clank/internal/agent"
	"github.com/supaclank/clank/internal/host"
	hostmux "github.com/supaclank/clank/internal/host/mux"
)

func newProjectsServer(t *testing.T) *httptest.Server {
	t.Helper()
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
	})
	t.Cleanup(svc.Shutdown)
	srv := httptest.NewServer(hostmux.New(svc, nil).Handler())
	t.Cleanup(srv.Close)
	return srv
}

// templateCloneURL builds a real local git repo and returns its file://
// clone URL, exercising the handler's full clone path without network.
func templateCloneURL(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("git", "init", "-b", "main")
	run("git", "config", "user.email", "t@t")
	run("git", "config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(dir, "App.tsx"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("git", "add", ".")
	run("git", "commit", "-m", "seed")
	return "file://" + dir
}

func TestHandleCreateProject(t *testing.T) {
	workRoot := filepath.Join(t.TempDir(), "work")
	prev := host.SetWorkRootForTest(workRoot)
	t.Cleanup(func() { host.SetWorkRootForTest(prev) })

	srv := newProjectsServer(t)
	body, _ := json.Marshal(map[string]string{"clone_url": templateCloneURL(t), "name": "my-app"})
	resp, err := http.Post(srv.URL+"/projects/create", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var out host.CreateWorktreeResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.WorktreeID == "" || out.DisplayName != "my-app" || out.Branch != "main" {
		t.Fatalf("unexpected result: %+v", out)
	}
}

func TestHandleCreateProject_MissingFields(t *testing.T) {
	srv := newProjectsServer(t)
	cases := map[string]string{
		"missing name":      `{"clone_url":"file:///tmp/x"}`,
		"missing clone_url": `{"name":"app"}`,
		"empty body":        `{}`,
	}
	for label, payload := range cases {
		t.Run(label, func(t *testing.T) {
			resp, err := http.Post(srv.URL+"/projects/create", "application/json", strings.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}
