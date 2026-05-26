//go:build unix

package preview

import (
	"context"
	"os/exec"
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
		Kind:        KindExpo,
		CmdTemplate: fakeMetroScript(""),
		ReadyProbe:  expoReadyProbe,
	}
	r, err := spawn(ctx, spawnRequest{
		WorkDir:        t.TempDir(),
		Spec:           spec,
		PreviewURLBase: "http://localhost:8080/preview/test",
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	t.Cleanup(func() { r.stopWithGrace(2 * time.Second) })

	waitForState(t, r, StateReady, 5*time.Second)
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
		Kind:        KindExpo,
		CmdTemplate: fakeMetroScript("(sleep 30 &)"),
		ReadyProbe:  expoReadyProbe,
	}
	r, err := spawn(ctx, spawnRequest{
		WorkDir:        t.TempDir(),
		Spec:           spec,
		PreviewURLBase: "http://localhost:8080/preview/test",
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
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
		Kind:        KindExpo,
		CmdTemplate: []string{"sh", "-c", "sleep 30"}, // never serves /status
		ReadyProbe:  expoReadyProbe,
	}
	r, err := spawn(ctx, spawnRequest{
		WorkDir:        t.TempDir(),
		Spec:           spec,
		PreviewURLBase: "http://localhost:8080/preview/test",
		ReadyTimeout:   500 * time.Millisecond,
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

