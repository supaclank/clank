//go:build !windows

package main

import "syscall"

// daemonSysProcAttr returns the SysProcAttr for forking the daemon as a
// new process group on Unix systems.
func daemonSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Setsid: true, // Create a new session, detach from terminal.
	}
}
