// Startup auto-discover: backfill RepoRef.Local for sessions whose
// path metadata was lost (e.g., orphaned by an old daemon migration or
// imported from a backend's own database without ever flowing through
// hub.createSession). Runs once after LoadSessions.
//
// We NEVER auto-delete sessions here. Orphans (sessions whose
// ExternalID does not match any current backend snapshot) are simply
// WARN-logged so the operator can decide whether to forget them.
package hub

import (
	"context"
	"os"
)

// runStartupDiscover queries every backend on every registered host
// for its current sessions and backfills GitRef.Local on any
// in-memory session that matches by ExternalID but lacks a local path.
//
// Called as a background goroutine from Run() so it doesn't block
// listener readiness. Idempotent — safe to run on every startup.
func (s *Service) runStartupDiscover(ctx context.Context) {
	// Pick a seed directory. OpenCode's DiscoverSessions needs a real
	// on-disk path to start the seed server; the home dir always
	// exists and the server's project list returns ALL known projects
	// regardless of where the server itself was launched.
	seed, err := os.UserHomeDir()
	if err != nil {
		s.log.Printf("startup-discover: cannot resolve home dir: %v", err)
		return
	}

	// Build externalID → directory across every (host, backend) we know.
	type snapKey struct {
		hostname string
		extID    string
	}
	resolved := make(map[snapKey]string)

	for hostname, h := range s.snapshotHosts() {
		backends, err := h.Backends(ctx)
		if err != nil {
			s.log.Printf("startup-discover: list backends on %s: %v", hostname, err)
			continue
		}
		for _, bi := range backends {
			snaps, err := h.Backend(bi.Name).Discover(ctx, seed)
			if err != nil {
				s.log.Printf("startup-discover: discover %s on %s: %v", bi.Name, hostname, err)
				continue
			}
			for _, snap := range snaps {
				if snap.ID == "" || snap.Directory == "" {
					continue
				}
				resolved[snapKey{string(hostname), snap.ID}] = snap.Directory
			}
		}
	}

	// Backfill any orphan sessions under the lock.
	var orphans []string
	s.mu.Lock()
	for id, ms := range s.sessions {
		if ms.info.ExternalID == "" {
			continue
		}
		if ms.info.GitRef.LocalPath != "" || ms.info.GitRef.RemoteURL != "" {
			continue
		}
		dir, ok := resolved[snapKey{ms.info.Hostname, ms.info.ExternalID}]
		if !ok {
			orphans = append(orphans, id)
			continue
		}
		ms.info.GitRef.LocalPath = dir
		s.persistSession(ms)
	}
	s.mu.Unlock()

	for _, id := range orphans {
		s.log.Printf("startup-discover: WARN orphan session %s — no backend snapshot matches its ExternalID; left as-is", id)
	}
}
