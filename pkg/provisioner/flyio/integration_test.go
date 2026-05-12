package flyio_test

// Real-sprite integration tests. Skipped unless SPRITES_TOKEN is in
// the env so default `go test ./...` stays fast and sandbox-free.
//
//   SPRITES_TOKEN=$YOUR_TOKEN go test -count=1 -run TestIntegration_FlyIO ./internal/provisioner/flyio/...

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sprites "github.com/superfly/sprites-go"

	"github.com/acksell/clank/internal/store"
	"github.com/acksell/clank/pkg/auth"
	"github.com/acksell/clank/pkg/gateway"
	"github.com/acksell/clank/pkg/provisioner"
	"github.com/acksell/clank/pkg/provisioner/flyio"
)

// newGatewayHandler builds a real gateway.Gateway in front of prov so
// the test exercises the same proxy path daemoncli mounts.
func newGatewayHandler(prov provisioner.Provisioner) (http.Handler, error) {
	gw, err := gateway.NewGateway(gateway.Config{
		Provisioner: prov,
	}, nil)
	if err != nil {
		return nil, err
	}
	return auth.Middleware(gw.Handler(), &auth.AllowAll{UserID: "local"}), nil
}

// loadFlyIOCreds returns Sprites creds from SPRITES_TOKEN, falling
// back to CLANK_DIR/preferences.json on non-short runs. Skips when
// neither yields a token.
func loadFlyIOCreds(t *testing.T) (token, org string) {
	t.Helper()
	if v := os.Getenv("SPRITES_TOKEN"); v != "" {
		return v, os.Getenv("SPRITES_ORG")
	}
	// Skip the preferences.json fallback in short mode so default
	// `go test ./...` stays fast for devs with cloud-hub set up.
	if testing.Short() {
		t.Skip("integration test: -short specified; set SPRITES_TOKEN to run")
	}
	dir := os.Getenv("CLANK_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".clank-cloud")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "preferences.json"))
	if err != nil {
		t.Skip("integration test: SPRITES_TOKEN not set and no preferences.json — skipping")
	}
	var prefs struct {
		FlyIO struct {
			APIToken         string `json:"api_token"`
			OrganizationSlug string `json:"organization_slug"`
		} `json:"flyio"`
	}
	if err := json.Unmarshal(raw, &prefs); err != nil || prefs.FlyIO.APIToken == "" {
		t.Skip("integration test: no flyio.api_token in preferences.json — skipping")
	}
	return prefs.FlyIO.APIToken, prefs.FlyIO.OrganizationSlug
}

// Shared sprite name across integration tests — adopt-or-recreate
// rather than minting fresh ones that would leak on the account.
const integrationUserID = "test"
const integrationSpriteName = "clank-host-test"

// newIntegrationProvisioner builds a flyio.Provisioner against a real
// Sprites account, deleting any orphan sprite from a previous run.
func newIntegrationProvisioner(t *testing.T) *flyio.Provisioner {
	t.Helper()
	token, org := loadFlyIOCreds(t)
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// Best-effort cleanup before the test runs; a leftover sprite from
	// a crashed previous run would otherwise block resolveOrCreate.
	rawClient := sprites.New(token)
	deleteSprite := func(label string) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := rawClient.DeleteSprite(ctx, integrationSpriteName); err != nil && !strings.Contains(err.Error(), "404") {
			t.Logf("integration test: %s of %s: %v (continuing)", label, integrationSpriteName, err)
		}
	}
	deleteSprite("pre-cleanup")
	// Always destroy on exit so the sprite never lingers between runs
	// (no orphaned billing, no stale resource conflicting with the next
	// developer to run these tests). Registered before flyio.New so the
	// cleanup fires even if New fails.
	t.Cleanup(func() { deleteSprite("post-cleanup") })

	prov, err := flyio.New(flyio.Options{
		APIToken:         token,
		OrganizationSlug: org,
	}, st, nil)
	if err != nil {
		t.Fatalf("flyio.New: %v", err)
	}
	return prov
}

// TestIntegration_FlyIO_EventsRouteIsReachable provisions a sprite
// and verifies GET /events returns 200 + text/event-stream + a
// `event: connected` frame. Fails when the Sprites edge intercepts
// /events with its own 404 page (the production regression).
func TestIntegration_FlyIO_EventsRouteIsReachable(t *testing.T) {
	prov := newIntegrationProvisioner(t)
	t.Cleanup(prov.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	ref, err := prov.EnsureHost(ctx, integrationUserID)
	if err != nil {
		t.Fatalf("EnsureHost: %v", err)
	}
	if ref.URL == "" {
		t.Fatal("EnsureHost returned empty URL")
	}

	// /status first — isolates "host mux up" from "/events working".
	statusOK, statusBody := getRoute(t, ref, "/status")
	if statusOK != http.StatusOK {
		t.Fatalf("/status: HTTP %d, body=%q (host mux not responding)", statusOK, statusBody)
	}

	body, status, ct := getRouteStream(t, ref, "/events", 3*time.Second)
	if status != http.StatusOK {
		t.Errorf("/events: HTTP %d (expected 200), Content-Type=%q, body=%q", status, ct, snippet(string(body), 240))
	}
	if !strings.Contains(ct, "text/event-stream") {
		t.Errorf("/events Content-Type=%q (expected text/event-stream)", ct)
	}
	if !strings.Contains(string(body), "event: connected") {
		t.Errorf("/events did not emit `event: connected` frame; body=%q", snippet(string(body), 240))
	}
}

// TestIntegration_FlyIO_StatusRouteIsReachable sanity-checks the
// integration setup itself; isolates "test can reach sprite" from
// route-specific failures.
func TestIntegration_FlyIO_StatusRouteIsReachable(t *testing.T) {
	prov := newIntegrationProvisioner(t)
	t.Cleanup(prov.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	ref, err := prov.EnsureHost(ctx, integrationUserID)
	if err != nil {
		t.Fatalf("EnsureHost: %v", err)
	}
	status, body := getRoute(t, ref, "/status")
	if status != http.StatusOK {
		t.Fatalf("/status: HTTP %d, body=%q", status, snippet(body, 240))
	}
}

// TestIntegration_FlyIO_PingRouteIsReachable: /ping returns the
// running clank-host's version+PID — helpful when chasing stale-
// binary bugs.
func TestIntegration_FlyIO_PingRouteIsReachable(t *testing.T) {
	prov := newIntegrationProvisioner(t)
	t.Cleanup(prov.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	ref, err := prov.EnsureHost(ctx, integrationUserID)
	if err != nil {
		t.Fatalf("EnsureHost: %v", err)
	}
	status, body := getRoute(t, ref, "/ping")
	if status != http.StatusOK {
		t.Fatalf("/ping: HTTP %d, body=%q", status, snippet(body, 240))
	}
	t.Logf("/ping body: %s", body)
}

// TestIntegration_FlyIO_RealSpriteEventsRoute hits the user's actual
// cloud-hub sprite (clank-host-local) without recreating it — picks
// up whatever URL settings have accumulated across daemon versions.
//
// Skips if the row is missing in the store rather than cold-creating
// a sprite — that name (`clank-host-local`) collides with what the
// laptop daemon uses in production, and an integration test must
// never plant a sprite there. To run this test you need to have
// already provisioned via `make cloud-hub` (or equivalent).
//
//	CLANK_DIR=$HOME/.clank-cloud go test -count=1 -run TestIntegration_FlyIO_RealSpriteEventsRoute -v ./internal/provisioner/flyio/...
func TestIntegration_FlyIO_RealSpriteEventsRoute(t *testing.T) {
	token, org := loadFlyIOCreds(t)
	clankDir := os.Getenv("CLANK_DIR")
	if clankDir == "" {
		home, _ := os.UserHomeDir()
		clankDir = filepath.Join(home, ".clank-cloud")
	}
	dbPath := filepath.Join(clankDir, "clank.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("real-sprite test: %s missing — run `make cloud-hub` once first to provision", dbPath)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open user store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// Refuse to cold-create. If the row's missing, the sprite either
	// doesn't exist yet (run cloud-hub first) or it exists upstream
	// without a row pointing at it (corrupt state — fix manually rather
	// than letting a test claim it).
	skipCtx, skipCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer skipCancel()
	if _, err := st.GetHostByUser(skipCtx, "local", "flyio"); errors.Is(err, store.ErrHostNotFound) {
		t.Skipf("real-sprite test: no (local, flyio) row in %s — run cloud-hub first; refusing to cold-create a sprite named clank-host-local (collides with laptop-mode production)", dbPath)
	} else if err != nil {
		t.Fatalf("look up real sprite row: %v", err)
	}

	prov, err := flyio.New(flyio.Options{
		APIToken:         token,
		OrganizationSlug: org,
	}, st, nil)
	if err != nil {
		t.Fatalf("flyio.New: %v", err)
	}
	t.Cleanup(prov.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ref, err := prov.EnsureHost(ctx, "local")
	if err != nil {
		t.Fatalf("EnsureHost(local): %v", err)
	}
	t.Logf("real sprite URL: %s", ref.URL)

	body, status, ct := getRouteStream(t, ref, "/events", 3*time.Second)
	t.Logf("real sprite /events: HTTP %d, Content-Type=%q", status, ct)
	t.Logf("real sprite /events body snippet: %s", snippet(string(body), 240))
	if status != http.StatusOK {
		t.Errorf("real sprite /events returned HTTP %d (TUI bug repro)", status)
	}
}

// getRoute issues a short-timeout authenticated GET via ref.Transport.
func getRoute(t *testing.T, ref provisioner.HostRef, path string) (int, string) {
	t.Helper()
	cli := &http.Client{Transport: ref.Transport, Timeout: 10 * time.Second}
	url := strings.TrimRight(ref.URL, "/") + path
	resp, err := cli.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	return resp.StatusCode, string(body)
}

// getRouteStream is getRoute for SSE: caps body reads at dur so we
// don't block on connections held open for streaming.
func getRouteStream(t *testing.T, ref provisioner.HostRef, path string, dur time.Duration) (body []byte, status int, contentType string) {
	t.Helper()
	cli := &http.Client{Transport: ref.Transport}
	url := strings.TrimRight(ref.URL, "/") + path
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := cli.Do(req)
	if err != nil {
		// Body-read timeout is OK; we already have the response start.
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, 0, ""
		}
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	contentType = resp.Header.Get("Content-Type")
	status = resp.StatusCode
	body, _ = io.ReadAll(io.LimitReader(resp.Body, 4*1024))
	return body, status, contentType
}

// snippet shortens body strings for log readability.
func snippet(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// TestIntegration_FlyIO_GatewayProxyToRealSpriteEvents reproduces the
// TUI bug without cloudflared: mounts the real gateway in front of
// the real flyio provisioner and SSE-subscribes through it. Catches
// gateway-only failure modes (Host header, prefix stripping, transport
// chain, etc.) that the direct-sprite test doesn't.
//
//	CLANK_DIR=$HOME/.clank-cloud go test -count=1 -run TestIntegration_FlyIO_GatewayProxyToRealSpriteEvents -v ./internal/provisioner/flyio/...
func TestIntegration_FlyIO_GatewayProxyToRealSpriteEvents(t *testing.T) {
	token, org := loadFlyIOCreds(t)
	clankDir := os.Getenv("CLANK_DIR")
	if clankDir == "" {
		home, _ := os.UserHomeDir()
		clankDir = filepath.Join(home, ".clank-cloud")
	}
	dbPath := filepath.Join(clankDir, "clank.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("gateway-proxy test: %s missing — run `make cloud-hub` once first", dbPath)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open user store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// Same refusal as TestIntegration_FlyIO_RealSpriteEventsRoute: skip
	// rather than cold-create a sprite at the production name.
	skipCtx, skipCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer skipCancel()
	if _, err := st.GetHostByUser(skipCtx, "local", "flyio"); errors.Is(err, store.ErrHostNotFound) {
		t.Skipf("gateway-proxy test: no (local, flyio) row in %s — run cloud-hub first; refusing to cold-create clank-host-local", dbPath)
	} else if err != nil {
		t.Fatalf("look up real sprite row: %v", err)
	}

	prov, err := flyio.New(flyio.Options{
		APIToken:         token,
		OrganizationSlug: org,
	}, st, nil)
	if err != nil {
		t.Fatalf("flyio.New: %v", err)
	}
	t.Cleanup(prov.Stop)

	gw, err := newGatewayHandler(prov)
	if err != nil {
		t.Fatalf("gateway: %v", err)
	}
	srv := httptest.NewServer(gw)
	t.Cleanup(srv.Close)
	t.Logf("gateway listening on %s; proxying to real flyio sprite for user 'local'", srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events", nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /events through gateway: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
	t.Logf("gateway → /events: HTTP %d, Content-Type=%q", resp.StatusCode, resp.Header.Get("Content-Type"))
	t.Logf("gateway → /events body snippet: %s", snippet(string(body), 240))
	if resp.StatusCode != http.StatusOK {
		t.Errorf("gateway → /events returned HTTP %d (matches TUI bug)", resp.StatusCode)
	}
}

// Keep the SDK import live for future tests that need direct access.
var _ = sprites.Sprite{}
