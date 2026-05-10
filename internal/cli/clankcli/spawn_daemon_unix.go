//go:build !windows

package clankcli

import "syscall"

// detachSysProcAttr returns the SysProcAttr for forking clankd into a
// new session so it survives the parent (TUI) exiting and doesn't share
// the terminal's signal-delivery group.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
