package daemoncli

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestStartHost_SpawnsAndServes exercises the full supervisor path:
//   - builds the clank-host binary into a temp dir
//   - calls startHost which spawns it on a Unix socket in another temp dir
//   - issues a real catalog request (ListBackends) through hostclient.HTTP
//   - calls stop() and asserts the socket file is removed and the
//     subprocess exited
//
// This is the only test that goes through:
//
//	real Unix socket  ←→  spawned subprocess
//
// All in-process integration tests use httptest TCP.
func TestStartHost_SpawnsAndServes(t *testing.T) {
	if testing.Short() {
		t.Skip("requires building clank-host binary")
	}

	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "clank-host")

	// Build clank-host into the temp dir. Reusing the same Go toolchain
	// the test runs under guarantees the binary matches the current source.
	build := exec.Command("go", "build", "-o", binPath, "../../../cmd/clank-host")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build clank-host: %v", err)
	}

	// Override binary resolution to point at the just-built binary.
	t.Setenv("CLANK_HOST_BIN", binPath)

	configDir := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	hh, err := startHost(ctx, configDir, io.Discard)
	if err != nil {
		t.Fatalf("startHost: %v", err)
	}

	// Sanity: socket exists.
	if _, err := os.Stat(hh.socketPath); err != nil {
		t.Fatalf("socket %s missing after startHost: %v", hh.socketPath, err)
	}

	// Real wire call. ListBackends returns the two managers clank-host
	// constructs in main(); empty result would mean either backends
	// failed to initialize OR the wire is broken.
	backends, err := hh.client.Backends(ctx)
	if err != nil {
		t.Fatalf("ListBackends over Unix socket: %v", err)
	}
	if len(backends) != 2 {
		t.Fatalf("expected 2 backends from clank-host, got %d: %+v", len(backends), backends)
	}

	pid := hh.cmd.Process.Pid
	hh.stop()

	// Socket file should be cleaned up.
	if _, err := os.Stat(hh.socketPath); !os.IsNotExist(err) {
		t.Errorf("socket %s should be removed after stop, stat err=%v", hh.socketPath, err)
	}

	// Subprocess should be reaped. Sending signal 0 to a reaped pid
	// returns ESRCH on most Unixes; we just check the cmd's ProcessState.
	if hh.cmd.ProcessState == nil {
		t.Errorf("subprocess (pid=%d) ProcessState is nil after stop — not waited?", pid)
	}
}

// TestResolveHostBinary_EnvOverride verifies CLANK_HOST_BIN takes
// precedence over PATH lookup. Regression guard: the env var is the
// only injection point tests have.
func TestResolveHostBinary_EnvOverride(t *testing.T) {
	tmp := t.TempDir()
	fake := filepath.Join(tmp, "fake-clank-host")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	t.Setenv("CLANK_HOST_BIN", fake)

	got, err := resolveHostBinary()
	if err != nil {
		t.Fatalf("resolveHostBinary: %v", err)
	}
	if got != fake {
		t.Errorf("expected %s, got %s", fake, got)
	}
}

// TestResolveHostBinary_MissingEnvFile fails fast rather than falling
// back to PATH. AGENTS.md: prefer fast failures over silent fallbacks.
func TestResolveHostBinary_MissingEnvFile(t *testing.T) {
	t.Setenv("CLANK_HOST_BIN", "/nonexistent/clank-host-xyz")
	_, err := resolveHostBinary()
	if err == nil {
		t.Fatal("expected error for missing CLANK_HOST_BIN file, got nil")
	}
}
