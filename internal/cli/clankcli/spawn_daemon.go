package clankcli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/acksell/clank/internal/config"
	daemonclient "github.com/acksell/clank/internal/daemonclient"
)

// daemonStartTimeout caps how long we wait for the spawned clankd to
// answer /ping before declaring startup failed.
const daemonStartTimeout = 5 * time.Second

// spawnLocalDaemon starts the local clankd in the background and waits
// for its Unix socket to become reachable.
//
// We deliberately do NOT re-exec the current process: this is called
// from `clank`, which has no `start` subcommand — that lives only in
// `clankd`. Locating clankd explicitly keeps the laptop binary's
// auto-start path independent of which subcommands `clank` exposes.
func spawnLocalDaemon() error {
	clankd, err := findClankd()
	if err != nil {
		return err
	}

	dir, err := config.Dir()
	if err != nil {
		return fmt.Errorf("config dir: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir config dir: %w", err)
	}
	logPath := filepath.Join(dir, "daemon.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("open daemon log: %w", err)
	}

	cmd := exec.Command(clankd, "start", "--foreground")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = detachSysProcAttr()
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("spawn %s: %w", clankd, err)
	}
	logFile.Close()

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	client, err := daemonclient.NewLocalClient()
	if err != nil {
		return fmt.Errorf("create client: %w", err)
	}

	deadline := time.After(daemonStartTimeout)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()

	for {
		pctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		pingErr := client.Ping(pctx)
		cancel()
		if pingErr == nil {
			fmt.Printf("Daemon started (pid=%d)\n", cmd.Process.Pid)
			return nil
		}
		select {
		case waitErr := <-exited:
			return fmt.Errorf("clankd exited during startup: %v (see %s)", waitErr, logPath)
		case <-deadline:
			return fmt.Errorf("clankd (pid=%d) not reachable after %v (see %s)", cmd.Process.Pid, daemonStartTimeout, logPath)
		case <-tick.C:
		}
	}
}

// findClankd locates the clankd binary for spawning. Prefers a clankd
// installed next to the running clank binary so that paired installs
// (e.g. `go install ./cmd/clank/... ./cmd/clankd/...` into one bin dir)
// pick up the matching daemon, then falls back to the first clankd on
// PATH.
func findClankd() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("find executable: %w", err)
	}
	return findClankdFor(self)
}

func findClankdFor(self string) (string, error) {
	name := "clankd"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	sibling := filepath.Join(filepath.Dir(self), name)
	if info, err := os.Stat(sibling); err == nil && !info.IsDir() {
		return sibling, nil
	}
	if found, err := exec.LookPath("clankd"); err == nil {
		return found, nil
	}
	return "", fmt.Errorf("clankd binary not found next to %s or on PATH (try `go install github.com/acksell/clank/cmd/clankd@latest`)", self)
}
