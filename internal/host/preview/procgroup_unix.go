//go:build unix

package preview

import (
	"os/exec"
	"syscall"
	"time"
)

// configureProcessGroup wires the child to a fresh process group so we
// can kill the whole tree on Stop. Without this, Metro's forked Node
// workers (bundler, HMR server, file watcher) survive a parent SIGKILL
// and become PID 1 orphans — the documented anti-pattern in
// https://github.com/anthropics/claude-code/issues/50544 and
// https://github.com/anthropics/claude-code/issues/16198.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Graceful context-cancel: when the spawn context is canceled (readiness
	// timeout, or the caller bailing out), SIGTERM the whole group so
	// npm/Metro unwind cleanly instead of being SIGKILL'd mid-write — a
	// half-extracted node_modules is exactly the corruption we're guarding
	// against. Go's WaitDelay then escalates to its default SIGKILL if the
	// tree hasn't exited within the grace window.
	cmd.Cancel = func() error {
		if cmd.Process == nil || cmd.Process.Pid <= 0 {
			return nil
		}
		// Negative pid targets the whole group (Setpgid above). ESRCH
		// (group already gone) is fine. Pid guard prevents negating a
		// zero or negative pid into a dangerous Kill target.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = gracefulCancelDelay
}

// stopProcessGroup sends SIGTERM to the entire group, waits up to
// gracePeriod for it to exit (signalled by done closing), and then
// escalates to SIGKILL. Returns once the wait goroutine has reaped the
// child — callers can assume the process tree is gone on return.
//
// pgid==0 means "no process spawned" or "process already reaped"; the
// caller MUST hold the lock that guards r.pgid. We tolerate a stale
// pgid because Kill(0) is a no-op once the group is empty.
func stopProcessGroup(pgid int, done <-chan struct{}, gracePeriod time.Duration) {
	if pgid == 0 {
		return
	}
	// Negative pid targets the whole group. ESRCH (no such process) is
	// fine — means the group already exited between Start and Stop.
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	select {
	case <-done:
		return
	case <-time.After(gracePeriod):
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	<-done
}
