package hostmux

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
	"github.com/acksell/clank/internal/host/hosttest"
)

// previewTestEnv stands up a real *host.Service rooted at a temp
// $HOME/work, with one fixture worktree pre-staged at <work>/<id>/.
// The mux is wrapped in an httptest.Server so callers can talk to it
// with a real http.Client (the HMR proxy needs that; httptest.Recorder
// can't drive WebSocket upgrades).
//
// NOTE: these tests do NOT call t.Parallel(). The host package keeps
// workRoot as a process-global mutated by SetWorkRootForTest (see the
// TODO in internal/host/service.go) — parallelizing here races. The
// mux preview suite is sequential by design until that global gets a
// proper RWMutex.
type previewTestEnv struct {
	worktreeID string
	srv        *httptest.Server
	mux        *Mux
}

func newPreviewTestEnv(t *testing.T, files map[string]string) *previewTestEnv {
	t.Helper()
	workRoot := t.TempDir()
	prev := host.SetWorkRootForTest(workRoot)
	t.Cleanup(func() { host.SetWorkRootForTest(prev) })

	// Unique per-test so a leaked dir from a prior run can't reappear
	// as a polluted fixture (we hit this with a shared "01TESTPREVIEW..."
	// constant before — Detect saw stale Expo files).
	wid := "wt-" + strings.ReplaceAll(t.Name(), "/", "-")
	wdir := filepath.Join(workRoot, wid)
	if err := os.MkdirAll(wdir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", wdir, err)
	}
	for rel, content := range files {
		path := filepath.Join(wdir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	// Stand up a Service with no real backends — preview is independent
	// of agent/session machinery. BackendManagers must be non-nil per
	// the panic check in host.New.
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{},
	})
	t.Cleanup(svc.Shutdown)

	m := New(svc, nil)
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)
	return &previewTestEnv{worktreeID: wid, srv: srv, mux: m}
}

// expoFixture returns the file map a freshly-staged Expo worktree
// needs to satisfy preview.Detect.
func expoFixture() map[string]string {
	return map[string]string{
		"package.json": `{"dependencies":{"expo":"~50.0.0"}}`,
		"app.json":     `{"expo":{"name":"fixture"}}`,
	}
}

func TestPreviewStatus_AvailableExpo(t *testing.T) {
	env := newPreviewTestEnv(t, expoFixture())

	resp, err := http.Get(env.srv.URL + "/worktrees/" + env.worktreeID + "/preview/status")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status code = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Available bool   `json:"available"`
		Kind      string `json:"kind"`
		State     string `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Available {
		t.Errorf("available = false; want true for Expo fixture")
	}
	if body.Kind != "expo" {
		t.Errorf("kind = %q; want expo", body.Kind)
	}
	if body.State != "stopped" {
		t.Errorf("state = %q; want stopped (no spawn yet)", body.State)
	}
}

func TestPreviewStatus_NotAvailable(t *testing.T) {
	// No package.json, no app.json — just an empty worktree dir.
	env := newPreviewTestEnv(t, nil)

	resp, err := http.Get(env.srv.URL + "/worktrees/" + env.worktreeID + "/preview/status")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Available         bool            `json:"available"`
		SetupRequired     bool            `json:"setup_required"`
		SetupPrompt       string          `json:"setup_prompt"`
		ProjectConfigPath string          `json:"project_config_path"`
		HostConfigPath    json.RawMessage `json:"host_config_path"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Available {
		t.Errorf("available = true; want false for non-Expo dir")
	}
	if !body.SetupRequired {
		t.Errorf("setup_required = false; want true for a project without launch config")
	}
	if body.SetupPrompt == "" || body.ProjectConfigPath == "" {
		t.Errorf("setup instructions are incomplete: %+v", body)
	}
	if len(body.HostConfigPath) != 0 {
		t.Errorf("status exposes removed host_config_path: %s", body.HostConfigPath)
	}
}

func TestPreviewStatus_SelectsConfiguredName(t *testing.T) {
	env := newPreviewTestEnv(t, map[string]string{
		"web/package.json":   `{}`,
		"admin/package.json": `{}`,
		".clank/launch.yaml": `default: web
previews:
  web:
    directory: web
    command: npm run dev -- --port "$PORT"
    ready:
      path: /
  admin:
    directory: admin
    command: npm run admin -- --port "$PORT"
    ready:
      path: /healthz
`,
	})

	resp, err := http.Get(env.srv.URL + "/worktrees/" + env.worktreeID + "/preview/status?name=admin")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Available   bool   `json:"available"`
		Kind        string `json:"kind"`
		ServiceName string `json:"service_name"`
		State       string `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.Available || body.Kind != "web" || body.ServiceName != "admin" || body.State != "stopped" {
		t.Errorf("status = %+v", body)
	}
}

func TestPreviewStartRejectsUnknownSelectionField(t *testing.T) {
	env := newPreviewTestEnv(t, expoFixture())
	resp, err := http.Post(env.srv.URL+"/worktrees/"+env.worktreeID+"/preview/start",
		"application/json", strings.NewReader(`{"service":"admin"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestDecodePreviewSelectionRejectsOversizedAndTrailingJSON(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		body string
	}{
		{name: "oversized", body: strings.Repeat(" ", previewSelectionMaxBytes+1)},
		{name: "trailing object", body: `{"name":"web"} {"name":"admin"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tt.body))
			if _, err := decodePreviewSelection(req); err == nil {
				t.Fatal("decodePreviewSelection: want error")
			}
		})
	}
}

func TestPreviewStatus_InvalidLaunchConfigYields422(t *testing.T) {
	env := newPreviewTestEnv(t, map[string]string{
		".clank/launch.yaml": "prevews: {}\n",
	})
	resp, err := http.Get(env.srv.URL + "/worktrees/" + env.worktreeID + "/preview/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	var body errResp
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Code != codePreviewConfigInvalid {
		t.Errorf("code = %q, want %q", body.Code, codePreviewConfigInvalid)
	}
}

func TestPreviewStart_SetupRequiredYields409(t *testing.T) {
	env := newPreviewTestEnv(t, nil) // no package.json

	// No body — preview/start no longer takes one (the gateway mints
	// the public URL; the sprite just spawns).
	resp, err := http.Post(env.srv.URL+"/worktrees/"+env.worktreeID+"/preview/start",
		"application/json", bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for missing launch setup", resp.StatusCode)
	}
	var body struct {
		Code              string          `json:"code"`
		SetupPrompt       string          `json:"setup_prompt"`
		ProjectConfigPath string          `json:"project_config_path"`
		HostConfigPath    json.RawMessage `json:"host_config_path"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Code != "preview_setup_required" {
		t.Errorf("code = %q; want preview_setup_required", body.Code)
	}
	if body.SetupPrompt == "" || body.ProjectConfigPath == "" {
		t.Errorf("setup error response is incomplete: %+v", body)
	}
	if len(body.HostConfigPath) != 0 {
		t.Errorf("setup error exposes removed host_config_path: %s", body.HostConfigPath)
	}
}

// The laptop preview key preserves the exact folder used for project context,
// including a repository subdirectory. Without a launch config, that folder
// produces the setup-required contract instead of being normalized elsewhere.
func TestPreviewStart_FolderSlugRequiresSetupAtSubdir(t *testing.T) {
	env := newPreviewTestEnv(t, nil)
	repo := hosttest.InitGitRepo(t)
	files := map[string]string{
		"package.json":     `{"devDependencies":{"vite":"^6.0.0"}}`,
		"sub/package.json": `{"dependencies":{"react":"^18"}}`,
	}
	for rel, content := range files {
		path := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	slug := host.LocalRepoSlug(filepath.Join(repo, "sub"))
	resp, err := http.Post(env.srv.URL+"/worktrees/"+slug+"/preview/start",
		"application/json", bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (setup required at the subdir)", resp.StatusCode)
	}
	var body errResp
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Code != "preview_setup_required" {
		t.Errorf("code = %q; want preview_setup_required", body.Code)
	}
}

// TestPreviewStatus_FolderSlug_ResolvesOutsideWorkRoot is the
// regression at the heart of the slug re-key: status/logs polling by
// the QR-carried key. With the old per-run ULID key, status resolution
// went through workDirFor and failed for any folder outside ~/work —
// the phone's loading screen had nothing to poll. A folder slug must
// resolve wherever the folder lives, git repo or not.
func TestPreviewStatus_FolderSlug_ResolvesOutsideWorkRoot(t *testing.T) {
	env := newPreviewTestEnv(t, nil)
	dir := t.TempDir() // outside the env's workRoot, not a git repo
	for rel, content := range expoFixture() {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	resp, err := http.Get(env.srv.URL + "/worktrees/" + host.LocalRepoSlug(dir) + "/preview/status")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a live folder slug", resp.StatusCode)
	}
	var body struct {
		Available bool   `json:"available"`
		Kind      string `json:"kind"`
		State     string `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Available || body.Kind != "expo" || body.State != "stopped" {
		t.Errorf("status = %+v; want Available=true, Kind=expo, State=stopped", body)
	}
}

// TestPreviewStatus_FolderSlugGone_Yields404 pins the moved/deleted
// folder answer: a slug that decodes cleanly but names nothing on disk
// is an honest 404, not a 500.
func TestPreviewStatus_FolderSlugGone_Yields404(t *testing.T) {
	env := newPreviewTestEnv(t, nil)
	gone := host.LocalRepoSlug(filepath.Join(t.TempDir(), "vanished"))

	resp, err := http.Get(env.srv.URL + "/worktrees/" + gone + "/preview/status")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a vanished folder", resp.StatusCode)
	}
}

func TestPreviewStop_NotRunningYields404(t *testing.T) {
	env := newPreviewTestEnv(t, expoFixture())

	resp, err := http.Post(env.srv.URL+"/worktrees/"+env.worktreeID+"/preview/stop",
		"application/json", bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var body errResp
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Code != "not_running" {
		t.Errorf("code = %q; want not_running", body.Code)
	}
}

func TestPreviewStart_UnknownWorktreeIDYields4xx(t *testing.T) {
	env := newPreviewTestEnv(t, expoFixture())

	// Use a worktree ID that has no on-disk directory under workRoot.
	resp, err := http.Post(env.srv.URL+"/worktrees/01UNKNOWN/preview/start",
		"application/json", bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	// workDirFor surfaces "worktree not present" via host.ErrNotFound,
	// which writeError maps to 404. Either way we want a client-error
	// status, not 500.
	if resp.StatusCode/100 != 4 {
		t.Fatalf("status = %d, want 4xx for unknown worktree", resp.StatusCode)
	}
}
