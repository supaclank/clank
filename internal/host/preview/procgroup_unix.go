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
