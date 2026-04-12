package daemon

import "github.com/acksell/clank/internal/agent"

// InjectSession exposes injectSession for external tests (package daemon_test).
func (d *Daemon) InjectSession(info agent.SessionInfo) { d.injectSession(info) }

// injectSession adds a session to the daemon's in-memory map without starting
// a backend. Used by tests via the export_test.go bridge.
func (d *Daemon) injectSession(info agent.SessionInfo) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sessions[info.ID] = &managedSession{info: info}
}
