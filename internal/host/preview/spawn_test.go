//go:build unix

package preview

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestSpawnReachesReady spawns a fake-Metro stub and waits for the
// HTTP /status probe to flip the state to Ready. Pure happy path.
func TestSpawnReachesReady(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	spec := Spec{
		Kind:                 KindExpo,
		CmdTemplate:          fakeMetroScript(""),
		ShouldSubstitutePort: true,
		ReadyProbe:           expoReadyProbe,
	}
	port, err := allocatePort()
	if err != nil {
		t.Fatalf("allocatePort: %v", err)
	}
	r, err := spawn(ctx, spawnRequest{
		WorkDir:     t.TempDir(),
		Spec:        spec,
		ServiceName: "default",
		Port:        port,
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	t.Cleanup(func() { r.stopWithGrace(2 * time.Second) })

	waitForState(t, r, StateReady, 5*time.Second)
}

func TestSpawnLogsConfiguredCommandBeforeChildOutput(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const configuredCommand = "npm ci && npm run dev"
	port, err := allocatePort()
	if err != nil {
		t.Fatalf("allocatePort: %v", err)
	}
	r, err := spawn(ctx, spawnRequest{
		WorkDir: t.TempDir(),
		Spec: Spec{
			Kind:              KindWeb,
			CmdTemplate:       []string{"sh", "-c", "echo child-boot; sleep 30"},
			StartupLogCommand: configuredCommand,
			ReadyProbe:        ReadyProbe{Path: "/"},
		},
		ServiceName: "web",
		Port:        port,
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	t.Cleanup(func() { r.stopWithGrace(2 * time.Second) })

	want := "$ " + configuredCommand + "\nchild-boot\n"
	waitForLogs(t, r, want, 5*time.Second)
}

// TestSpawnAndOrphanCleanup is the regression test for Change 2 of the
// plan: Setpgid + group SIGTERM/SIGKILL.
//
// Without process-group cleanup the backgrounded `sleep` survives the
// parent's SIGKILL and becomes PID 1 orphans — the documented anti-
// pattern that produced 7,400 zombies in CC Desktop #50544. This test
// proves the group-kill catches the child.
func TestSpawnAndOrphanCleanup(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Backgrounded sleep is a sibling — `(sleep 30 &)` detaches it
	// from the parent's wait but it remains in the same process group
	// because we set Setpgid before exec. The fake-Metro server then
	// runs (so the readiness probe passes), and we observe both
	// processes before tearing down.
	spec := Spec{
		Kind:                 KindExpo,
		CmdTemplate:          fakeMetroScript("(sleep 30 &)"),
		ShouldSubstitutePort: true,
		ReadyProbe:           expoReadyProbe,
	}
	port, err := allocatePort()
	if err != nil {
		t.Fatalf("allocatePort: %v", err)
	}
	r, err := spawn(ctx, spawnRequest{
		WorkDir:     t.TempDir(),
		Spec:        spec,
		ServiceName: "default",
		Port:        port,
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	t.Cleanup(func() { r.stopWithGrace(3 * time.Second) })
	waitForState(t, r, StateReady, 5*time.Second)

	pgid := r.pgid
	if pgid == 0 {
		t.Fatalf("expected non-zero pgid after spawn")
	}

	// Confirm there are at least 2 processes in the group (parent
	// shell + backgrounded sleep). On Linux the backgrounded sleep
	// usually shows up within ~100ms; poll briefly to let it land.
	if got := countProcessesInGroup(t, pgid, 1*time.Second, 2); got < 2 {
		t.Fatalf("expected ≥2 processes in group %d before stop, got %d", pgid, got)
	}

	// Stop via the production kill primitive.
	r.stopWithGrace(3 * time.Second)

	// All processes in the group should be gone. Allow a tiny window
	// for the kernel to reap zombies.
	if got := waitForGroupEmpty(t, pgid, 2*time.Second); got != 0 {
		t.Fatalf("expected 0 processes in group %d after stop, got %d (orphans leaked)", pgid, got)
	}
}

// TestSpawnReadinessTimeoutFails confirms the watchReady loop flips to
// Failed and tears the child down when the sentinel never appears.
func TestSpawnReadinessTimeoutFails(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	spec := Spec{
		Kind:                 KindExpo,
		CmdTemplate:          []string{"sh", "-c", "sleep 30"}, // never serves /status
		ShouldSubstitutePort: true,
		ReadyProbe:           expoReadyProbe,
	}
	port, err := allocatePort()
	if err != nil {
		t.Fatalf("allocatePort: %v", err)
	}
	r, err := spawn(ctx, spawnRequest{
		WorkDir:      t.TempDir(),
		Spec:         spec,
		ServiceName:  "default",
		Port:         port,
		ReadyTimeout: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	t.Cleanup(func() { r.stopWithGrace(1 * time.Second) })

	waitForState(t, r, StateFailed, 2*time.Second)
	// lastErr is written under r.mu by probeReady; happens-before via
	// waitForState's lock makes the unlocked read incidentally safe
	// today, but pin the contract explicitly so a future waitForState
	// refactor can't turn it into a race.
	r.mu.Lock()
	gotErr := r.lastErr
	r.mu.Unlock()
	if gotErr == "" {
		t.Errorf("expected lastErr to be set on readiness timeout")
	}
}

// waitForLogs polls r.logs until its snapshot equals want or deadline
// expires — the child writes its boot line asynchronously after Start.
func waitForLogs(t *testing.T, r *running, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := string(r.logs.Snapshot()); got == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("logs = %q, want %q", string(r.logs.Snapshot()), want)
}

// waitForState polls r.state until it matches want or deadline expires.
func waitForState(t *testing.T, r *running, want State, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		got := r.state
		r.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	r.mu.Lock()
	got := r.state
	r.mu.Unlock()
	t.Fatalf("state did not reach %s within %s (got %s)", want, timeout, got)
}

// countProcessesInGroup polls until at least minWanted processes are in
// pgid or timeout expires. Returns the highest count seen.
func countProcessesInGroup(t *testing.T, pgid int, timeout time.Duration, minWanted int) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	highest := 0
	for time.Now().Before(deadline) {
		n := psCountForGroup(t, pgid)
		if n > highest {
			highest = n
		}
		if n >= minWanted {
			return n
		}
		time.Sleep(50 * time.Millisecond)
	}
	return highest
}

// waitForGroupEmpty polls until pgrep reports 0 members in pgid, or the
// timeout expires. Returns the final count.
func waitForGroupEmpty(t *testing.T, pgid int, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		n := psCountForGroup(t, pgid)
		if n == 0 {
			return 0
		}
		time.Sleep(50 * time.Millisecond)
	}
	return psCountForGroup(t, pgid)
}

// TestBuildEnv_EmptyPublicURL_OmitsProxyVar pins the laptop-dev
// path: with no gateway wired, no PublicURL is threaded into spawn
// and buildEnv must NOT set EXPO_PACKAGER_PROXY_URL — Metro then
// advertises its localhost URL directly and the user opens it as-is.
// REACT_NATIVE_PACKAGER_HOSTNAME is never set (it can't override the
// port half — see Expo's UrlCreator.ts).
func TestBuildEnv_EmptyPublicURL_OmitsProxyVar(t *testing.T) {
	t.Parallel()
	env, err := buildEnv(environmentRequest{Kind: KindExpo, MarkerPath: "/tmp/marker.bun", Port: 5173})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range env {
		if strings.HasPrefix(e, "REACT_NATIVE_PACKAGER_HOSTNAME=") {
			t.Errorf("buildEnv set REACT_NATIVE_PACKAGER_HOSTNAME — overrides only the hostname half: %q", e)
		}
		if strings.HasPrefix(e, "EXPO_PACKAGER_PROXY_URL=") {
			t.Errorf("buildEnv with empty publicURL set EXPO_PACKAGER_PROXY_URL: %q", e)
		}
	}
	// Sanity: EXPO_NO_DOTENV is still set (Metro shouldn't load the
	// repo's .env into its own process).
	var sawNoDotenv bool
	for _, e := range env {
		if e == "EXPO_NO_DOTENV=1" {
			sawNoDotenv = true
			break
		}
	}
	if !sawNoDotenv {
		t.Error("buildEnv missing EXPO_NO_DOTENV=1")
	}
}

func TestBuildEnvPinsAllocatedPort(t *testing.T) {
	t.Setenv("PORT", "9999")
	env, err := buildEnv(environmentRequest{Kind: KindExpo, MarkerPath: "/tmp/marker.bun", Port: 5173})
	if err != nil {
		t.Fatal(err)
	}
	var ports []string
	for _, entry := range env {
		if strings.HasPrefix(entry, "PORT=") {
			ports = append(ports, entry)
		}
	}
	if len(ports) != 1 || ports[0] != "PORT=5173" {
		t.Fatalf("PORT entries = %q, want exactly [PORT=5173]", ports)
	}
}

func TestBuildEnvOmitsExpoBootstrapMarkerForConfiguredWeb(t *testing.T) {
	t.Parallel()

	env, err := buildEnv(environmentRequest{Kind: KindWeb, Port: 5173})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range env {
		if strings.HasPrefix(entry, bootstrapMarkerEnv+"=") {
			t.Fatalf("configured web environment contains Expo bootstrap marker: %q", entry)
		}
	}
}

func TestBuildEnvPreservesWebEnvironmentWithoutExpoOverrides(t *testing.T) {
	t.Setenv("CI", "preview-test")
	t.Setenv("EXPO_NO_DOTENV", "parent")

	env, err := buildEnv(environmentRequest{Kind: KindWeb, PublicURL: "https://preview.example.test", Port: 5173})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"CI":             "preview-test",
		"EXPO_NO_DOTENV": "parent",
		"PORT":           "5173",
	}
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			if expected, exists := want[key]; exists && value == expected {
				delete(want, key)
			}
		}
		if strings.HasPrefix(entry, "EXPO_PACKAGER_PROXY_URL=") {
			t.Errorf("configured web environment received Expo proxy URL: %q", entry)
		}
	}
	if len(want) != 0 {
		t.Errorf("configured web environment missing inherited values: %v", want)
	}
}

func TestBuildEnvRendersConfiguredWebEnvironmentWithPublicHostname(t *testing.T) {
	t.Setenv("__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS", "inherited.example.test")
	t.Setenv("CLANK_PREVIEW_PUBLIC_HOSTNAME", "inherited.example.test")

	env, err := buildEnv(environmentRequest{
		Kind:      KindWeb,
		PublicURL: "https://preview-token.dev.supaclank.dev/path",
		Port:      5173,
		Configured: map[string]string{
			"__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS": "${CLANK_PREVIEW_PUBLIC_HOSTNAME}",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"PORT":                                   "5173",
		"CLANK_PREVIEW_PUBLIC_HOSTNAME":          "preview-token.dev.supaclank.dev",
		"__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS": "preview-token.dev.supaclank.dev",
	}
	assertEnvironmentValues(t, env, want)
}

func TestBuildEnvUsesLoopbackHostnameWithoutGateway(t *testing.T) {
	t.Parallel()

	env, err := buildEnv(environmentRequest{Kind: KindWeb, Port: 5173})
	if err != nil {
		t.Fatal(err)
	}
	assertEnvironmentValues(t, env, map[string]string{
		"CLANK_PREVIEW_PUBLIC_HOSTNAME": "127.0.0.1",
	})
}

func TestResolvePreviewEndpointKeepsPortOutOfPublicHostname(t *testing.T) {
	t.Parallel()

	endpoint, err := resolvePreviewEndpoint(KindWeb, "https://preview.example.test:8443/path", 5173)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.PublicHostname != "preview.example.test" {
		t.Errorf("PublicHostname = %q", endpoint.PublicHostname)
	}
	if endpoint.ReadinessHost != "preview.example.test:8443" {
		t.Errorf("ReadinessHost = %q", endpoint.ReadinessHost)
	}
}

func TestBuildEnvRejectsNonHTTPPublicURL(t *testing.T) {
	t.Parallel()

	_, err := buildEnv(environmentRequest{Kind: KindWeb, PublicURL: "preview.example.test", Port: 5173})
	if err == nil || !strings.Contains(err.Error(), "absolute HTTP URL") {
		t.Fatalf("buildEnv error = %v, want invalid public URL", err)
	}
}

func TestProbeOnceSendsPublicHostHeader(t *testing.T) {
	t.Parallel()

	const publicHost = "preview-token.dev.supaclank.dev"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != publicHost {
			http.Error(w, "blocked host", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	if !probeOnce(server.Client(), server.URL, nil, publicHost) {
		t.Fatal("probeOnce did not use the public Host header")
	}
}

func TestSpawnConfiguredWebPassesPublicHostValidation(t *testing.T) {
	t.Parallel()

	const publicURL = "https://preview-token.dev.supaclank.dev/"
	serverScript := filepath.Join(t.TempDir(), "host_validating_server.py")
	if err := os.WriteFile(serverScript, []byte(`
import http.server, os

port = int(os.environ["PORT"])
allowed_host = os.environ["__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS"]

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.headers.get("Host") != allowed_host:
            self.send_response(403)
            self.end_headers()
            return
        self.send_response(200)
        self.end_headers()
    def log_message(self, *args):
        pass

http.server.HTTPServer(("127.0.0.1", port), Handler).serve_forever()
`), 0o600); err != nil {
		t.Fatal(err)
	}

	port, err := allocatePort()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	running, err := spawn(ctx, spawnRequest{
		WorkDir: t.TempDir(),
		Spec: Spec{
			Kind:        KindWeb,
			CmdTemplate: []string{"python3", serverScript},
			Environment: map[string]string{
				"__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS": "${CLANK_PREVIEW_PUBLIC_HOSTNAME}",
			},
			ReadyProbe: ReadyProbe{Path: "/"},
		},
		ServiceName: "web",
		Port:        port,
		PublicURL:   publicURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { running.stopWithGrace(2 * time.Second) })

	waitForState(t, running, StateReady, 5*time.Second)
}

func assertEnvironmentValues(t *testing.T, env []string, want map[string]string) {
	t.Helper()

	counts := make(map[string]int, len(want))
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		expected, exists := want[key]
		if !exists {
			continue
		}
		counts[key]++
		if value != expected {
			t.Errorf("%s = %q, want %q", key, value, expected)
		}
	}
	for key := range want {
		if counts[key] != 1 {
			t.Errorf("%s entries = %d, want exactly 1", key, counts[key])
		}
	}
}

func TestRenderSpecArgsPreservesConfiguredPercentFormatting(t *testing.T) {
	t.Parallel()

	command := `printf '%d' 7; exec server --port "$PORT"`
	args, err := renderSpecArgs(Spec{
		Kind:        KindWeb,
		CmdTemplate: []string{"sh", "-c", command},
	}, 5173)
	if err != nil {
		t.Fatal(err)
	}
	if args[2] != command {
		t.Fatalf("configured command = %q, want unchanged %q", args[2], command)
	}
}

// TestBuildEnv_OmitsCI pins the lesson from a debugging session: setting
// CI=true (or =1) in Metro's env makes Metro disable file watching +
// HMR ("Metro is running in CI mode, reloads are disabled"). The
// codebase MUST suppress interactive prompts via narrower flags
// (--non-interactive on the argv) instead.
// If we ever add CI here again, HMR silently breaks for every
// preview launch.
func TestBuildEnv_OmitsCI(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, url string }{
		{"empty", ""},
		{"with-public-url", "http://preview-abc.localhost:7878"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			env, err := buildEnv(environmentRequest{Kind: KindExpo, MarkerPath: "/tmp/marker.bun", PublicURL: c.url, Port: 5173})
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range env {
				if strings.HasPrefix(e, "CI=") {
					t.Errorf("buildEnv leaked CI to child process — disables Metro HMR: %q", e)
				}
			}
		})
	}
}

// TestBuildEnv_PublicURL_SetsProxyVar pins the gateway-wired path:
// the public URL minted by the gateway's register webhook gets
// threaded into EXPO_PACKAGER_PROXY_URL so Metro advertises the
// public host:port (gateway port, NOT Metro's internal listen port)
// in its manifest's hostUri + launchAsset.url. Without this the
// dev-launcher fetches the bundle from an unreachable port — the
// original symptom that motivated this regression test.
func TestBuildEnv_PublicURL_SetsProxyVar(t *testing.T) {
	t.Parallel()
	publicURL := "http://preview-abc.localhost:7878"
	env, err := buildEnv(environmentRequest{Kind: KindExpo, MarkerPath: "/tmp/marker.bun", PublicURL: publicURL, Port: 5173})
	if err != nil {
		t.Fatal(err)
	}
	want := "EXPO_PACKAGER_PROXY_URL=" + publicURL
	var saw bool
	for _, e := range env {
		if e == want {
			saw = true
			break
		}
	}
	if !saw {
		t.Errorf("buildEnv(%q) did not set %q in env", publicURL, want)
	}
}

// The shim's --require must MERGE into an inherited NODE_OPTIONS (not clobber
// it), and there must be exactly one NODE_OPTIONS entry. CLANK_PREVIEW_RUNTIME
// carries the runtime path. Not parallel: mutates the process env.
func TestBuildEnv_ShimRequireMergesNodeOptions(t *testing.T) {
	t.Setenv("NODE_OPTIONS", "--max-old-space-size=4096")
	env, err := buildEnv(environmentRequest{
		Kind:            KindExpo,
		MarkerPath:      "/tmp/marker.bun",
		ShimRequirePath: "/tmp/clank-preview/shim.js",
		RuntimePath:     "/tmp/clank-preview/runtime.js",
		Port:            5173,
	})
	if err != nil {
		t.Fatal(err)
	}

	var nodeOpts, runtime string
	nodeOptsCount := 0
	for _, e := range env {
		if strings.HasPrefix(e, "NODE_OPTIONS=") {
			nodeOpts = e
			nodeOptsCount++
		}
		if strings.HasPrefix(e, "CLANK_PREVIEW_RUNTIME=") {
			runtime = e
		}
	}
	if want := "NODE_OPTIONS=--max-old-space-size=4096 --require /tmp/clank-preview/shim.js"; nodeOpts != want {
		t.Errorf("NODE_OPTIONS not merged:\n got %q\nwant %q", nodeOpts, want)
	}
	if nodeOptsCount != 1 {
		t.Errorf("want exactly 1 NODE_OPTIONS entry, got %d", nodeOptsCount)
	}
	if want := "CLANK_PREVIEW_RUNTIME=/tmp/clank-preview/runtime.js"; runtime != want {
		t.Errorf("CLANK_PREVIEW_RUNTIME = %q, want %q", runtime, want)
	}
}

// TestSpawnThreadsPublicURLToChild proves the spawn → child env var
// path end-to-end: passing PublicURL on the spawnRequest results in
// the child process actually seeing EXPO_PACKAGER_PROXY_URL set on
// its own environment. This is the integration the dev-launcher
// depends on; without it the manifest URLs use Metro's internal port.
func TestSpawnThreadsPublicURLToChild(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	publicURL := "http://preview-thread.localhost:7878"
	envSentinel := t.TempDir() + "/child.env"

	// Fake-metro that records its EXPO_PACKAGER_PROXY_URL value to
	// disk then serves /status. The probe reads the file after Ready
	// to confirm the var was inherited.
	spec := Spec{
		Kind:                 KindExpo,
		ShouldSubstitutePort: true,
		CmdTemplate: []string{
			"sh", "-c",
			"printf '%s' \"${EXPO_PACKAGER_PROXY_URL}\" > " + envSentinel + " && " + fakeMetroBody(),
		},
		ReadyProbe: expoReadyProbe,
	}
	port, err := allocatePort()
	if err != nil {
		t.Fatalf("allocatePort: %v", err)
	}
	r, err := spawn(ctx, spawnRequest{
		WorkDir:     t.TempDir(),
		Spec:        spec,
		ServiceName: "default",
		Port:        port,
		PublicURL:   publicURL,
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	t.Cleanup(func() { r.stopWithGrace(2 * time.Second) })

	waitForState(t, r, StateReady, 5*time.Second)

	got, err := readSentinel(envSentinel)
	if err != nil {
		t.Fatalf("read env sentinel: %v", err)
	}
	if got != publicURL {
		t.Errorf("child saw EXPO_PACKAGER_PROXY_URL=%q, want %q", got, publicURL)
	}
}

// psCountForGroup uses `pgrep -g <pgid>` (or `ps -A` filtering on
// macOS) to count processes in the group. Returns 0 on any error so
// non-Unix builds simply skip the assertion path.
func psCountForGroup(t *testing.T, pgid int) int {
	t.Helper()
	// `pgrep -g <pgid>` works on both Linux and macOS.
	cmd := exec.Command("pgrep", "-g", strconv.Itoa(pgid))
	out, err := cmd.Output()
	if err != nil {
		// pgrep exits 1 when no process matches — that's our 0 count,
		// not a real error. Distinguish via *exec.ExitError.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return 0
		}
		t.Fatalf("pgrep -g %d: %v", pgid, err)
	}
	pids := strings.Fields(string(out))
	return len(pids)
}
