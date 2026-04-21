// Package hub is the control plane of Clank's two-process architecture.
//
// hub.Service owns:
//   - the host catalog (one or more clank-host plane endpoints)
//   - the session registry (Hub-side view; the authoritative routing target)
//   - event fanout to connected clients (TUI, CLI)
//   - the permission broker
//   - persistence (SQLite store)
//
// In production, clankd constructs a hub.Service, wires HTTP routes via
// internal/hub/mux, opens the listening Unix socket itself (see
// internal/cli/daemoncli), and supervises a clank-host child whose Unix
// socket gets registered as the "local" host through a *hostclient.HTTP.
// See hub_host_refactor.md.
//
// This file holds only the multi-host catalog primitives. The Service
// struct itself, its constructor, and the bulk of its methods live in
// service.go and the topical files (sessions.go, events.go, ...).
package hub

import (
	"fmt"
	"sort"

	"github.com/acksell/clank/internal/host"
	hostclient "github.com/acksell/clank/internal/host/client"
)

// RegisterHost adds a host to the catalog. The Service does not take
// ownership of the lifetime of c; the caller (e.g. clankd's host
// supervisor) decides when to spawn/kill the underlying process.
//
// Per Decision #3 of the refactor, the catalog is typed concretely on
// *hostclient.HTTP — there is no Host Go interface and no in-process
// shortcut. Tests stand up a real host.Service behind an httptest
// server and register the resulting HTTP client; production registers
// the supervisor's HTTP client over a Unix socket.
//
// Re-registering the same Hostname replaces the previous entry and
// returns the prior client so the caller can decide whether to Close
// it. Returns nil for the prior client when the registration was new.
// The Service does not Close clients itself — the supervisor that
// constructed them owns their lifetime.
func (s *Service) RegisterHost(id host.Hostname, c *hostclient.HTTP) (*hostclient.HTTP, error) {
	if id == "" {
		return nil, fmt.Errorf("host id is required")
	}
	if c == nil {
		return nil, fmt.Errorf("host client is required")
	}
	s.hostsMu.Lock()
	prev := s.hosts[id]
	s.hosts[id] = c
	s.hostsMu.Unlock()
	return prev, nil
}

// UnregisterHost removes a host from the catalog. Returns the client so the
// caller can decide whether to Close it. Returns nil if not registered.
func (s *Service) UnregisterHost(id host.Hostname) *hostclient.HTTP {
	s.hostsMu.Lock()
	defer s.hostsMu.Unlock()
	c, ok := s.hosts[id]
	if !ok {
		return nil
	}
	delete(s.hosts, id)
	return c
}

// Host returns the client for the given Hostname. The boolean is false if
// the host is not registered.
func (s *Service) Host(id host.Hostname) (*hostclient.HTTP, bool) {
	s.hostsMu.RLock()
	defer s.hostsMu.RUnlock()
	c, ok := s.hosts[id]
	return c, ok
}

// hostFor resolves a hostname to a registered host client. Empty hostname
// defaults to "local" — the default for sessions/requests that omit the
// field, matching the json-tag default in agent.StartRequest. Returns an
// error if the host is not in the catalog so callers fail fast rather
// than nil-deref the way the deleted s.hostClient shortcut would have.
func (s *Service) hostFor(hostname string) (*hostclient.HTTP, error) {
	id := host.Hostname(hostname)
	if id == "" {
		id = host.HostLocal
	}
	c, ok := s.Host(id)
	if !ok {
		return nil, fmt.Errorf("hub: host %q not registered", id)
	}
	return c, nil
}

// Hosts returns a snapshot of all registered host IDs, sorted
// lexicographically so callers (UI, logs, tests) get a deterministic
// order without having to sort themselves.
func (s *Service) Hosts() []host.Hostname {
	s.hostsMu.RLock()
	defer s.hostsMu.RUnlock()
	ids := make([]host.Hostname, 0, len(s.hosts))
	for id := range s.hosts {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// snapshotHosts returns a copy of the host map, taken under the read lock,
// so callers can iterate without holding the lock during external calls.
func (s *Service) snapshotHosts() map[host.Hostname]*hostclient.HTTP {
	s.hostsMu.RLock()
	defer s.hostsMu.RUnlock()
	out := make(map[host.Hostname]*hostclient.HTTP, len(s.hosts))
	for id, c := range s.hosts {
		out[id] = c
	}
	return out
}
