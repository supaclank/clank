// Package hostclient is the Hub-side handle for talking to a Host.
//
// Per Decision #3 of the hub/host refactor, there is no Go interface
// here and no in-process shortcut. The Hub always speaks HTTP to a
// Host — over a Unix socket for the local clankd↔clank-host case, and
// over TCP+TLS for managed remote hosts. Tests stand up a real
// host.Service behind an httptest.NewServer wrapped in
// internal/host/mux and connect via NewHTTP, so the wire shape under
// test matches production exactly.
package hostclient

import "github.com/acksell/clank/internal/agent"

// Compile-time guarantee that the HTTP-side session adapter satisfies
// the SessionBackend interface.
var _ agent.SessionBackend = (*httpSessionBackend)(nil)
