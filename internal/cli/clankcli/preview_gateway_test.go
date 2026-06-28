package clankcli

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"testing"
	"time"

	daemonclient "github.com/acksell/clank/internal/daemonclient"
)

// TestPreviewGatewayPairingAuth boots a real ephemeral preview gateway and
// asserts the pairing token gates access: the minted token reaches /ping,
// a missing or wrong token gets 401. Exercises the gateway boot, the
// auth.StaticBearer middleware, the TCP listener, and clean teardown — no
// mocks. /ping is served by the gateway itself, so this never spawns
// clank-host (no node/expo needed in CI).
func TestPreviewGatewayPairingAuth(t *testing.T) {
	t.Parallel()
	gw, err := startPreviewGateway(net.IPv4(127, 0, 0, 1), 0, t.TempDir(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("startPreviewGateway: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		gw.Shutdown(ctx)
	}()

	if gw.Token == "" {
		t.Fatal("gateway minted an empty pairing token")
	}

	// Correct token → reachable.
	authed := daemonclient.NewTCPClient(gw.BaseURL, gw.Token)
	if err := waitGatewayReady(authed); err != nil {
		t.Fatalf("authed client could not reach gateway: %v", err)
	}

	// No token → 401.
	resp, err := http.Get(gw.BaseURL + "/ping")
	if err != nil {
		t.Fatalf("GET /ping: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-bearer /ping = %d, want 401", resp.StatusCode)
	}

	// Wrong token → unauthorized (Ping returns an error on non-200).
	wrong := daemonclient.NewTCPClient(gw.BaseURL, "not-the-pairing-token")
	if err := wrong.Ping(context.Background()); err == nil {
		t.Fatal("wrong-bearer ping succeeded, want failure")
	}
}

// TestPreviewGatewayShutdownIsClean asserts Shutdown is safe even when no
// clank-host was ever spawned (no proxied request happened), so a user who
// Ctrl+C's before scanning doesn't hit a panic or hang.
func TestPreviewGatewayShutdownIsClean(t *testing.T) {
	t.Parallel()
	gw, err := startPreviewGateway(net.IPv4(127, 0, 0, 1), 0, t.TempDir(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("startPreviewGateway: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gw.Shutdown(ctx) // must not panic or block
}
