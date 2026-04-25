// Startup auto-discover: backfill RepoRef.Local for sessions whose
// path metadata was lost (e.g., orphaned by an old daemon migration or
// imported from a backend's own database without ever flowing through
// hub.createSession). Runs once after LoadSessions.
//
// Also heals the persisted info.Backend tag for sessions that were
// written by an older daemon that hardcoded BackendOpenCode for every
// discovered snapshot regardless of source. Without this, reopening a
// Claude session after a daemon restart would dispatch through the
// OpenCode backend manager (wrong work dir, wrong protocol) and the
// TUI would hang on "Waiting for agent output...".
//
// We NEVER auto-delete sessions here. Orphans (sessions whose
// ExternalID does not match any current backend snapshot) are simply
// WARN-logged so the operator can decide whether to forget them.
package hub

import (
	"context"
	"os"

	"github.com/acksell/clank/internal/agent"
)

// runStartupDiscover queries every backend on every registered host
// for its current sessions and (a) backfills GitRef.Local on any
// in-memory session that matches by ExternalID but lacks a local path
// and (b) heals info.Backend on any session whose persisted backend
// disagrees with the snapshot's source backend.
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

	// Build externalID → (directory, backend) across every (host, backend)
	// we know. Backend attribution is required so we can heal mis-tagged
	// rows written by older daemon versions.
	type snapKey struct {
		hostname string
		extID    string
	}
	type snapVal struct {
		directory string
		backend   agent.BackendType
	}
	resolved := make(map[snapKey]snapVal)

	for hostname, h := range s.snapshotHosts() {
		backends, err := h.Backends(ctx)
		if err != nil {
			s.log.Printf("startup-discover: list backends on %s: %v", hostname, err)
			continue
		}
		for _, bi := range backends {
			// Two-phase discovery per backend:
			//   1. Empty seed → AllSessionDiscoverer (Claude). Lets us
			//      heal sessions whose persisted GitRef.LocalPath is
			//      stale or wrong (e.g. mis-tagged backend rows from an
			//      older daemon).
			//   2. Home-dir seed → SessionDiscoverer (OpenCode). The
			//      seed server lists every project it knows, so one
			//      call covers all projects without iterating.
			// Backends that don't implement AllSessionDiscoverer return
			// (nil, nil) for the empty-seed call (host.Service routes
			// it that way), so the call is cheap.
			for _, seedDir := range []string{"", seed} {
				snaps, err := h.Backend(bi.Name).Discover(ctx, seedDir)
				if err != nil {
					s.log.Printf("startup-discover: discover %s on %s (seed=%q): %v", bi.Name, hostname, seedDir, err)
					continue
				}
				for _, snap := range snaps {
					if snap.ID == "" {
						continue
					}
					if snap.Backend == "" {
						// A backend manager that doesn't tag its
						// snapshots can't be used for healing — skip
						// rather than silently mis-attribute.
						s.log.Printf("startup-discover: WARN snapshot extID=%s from backend=%s has empty Backend; skipping", snap.ID, bi.Name)
						continue
					}
					// Don't overwrite a previously-resolved snapshot that
					// has a real Directory with one that doesn't (the
					// per-project DiscoverSessions has more accurate
					// Directory metadata than DiscoverAllSessions for
					// backends where Cwd may be missing).
					key := snapKey{string(hostname), snap.ID}
					if existing, ok := resolved[key]; ok && existing.directory != "" && snap.Directory == "" {
						continue
					}
					resolved[key] = snapVal{
						directory: snap.Directory,
						backend:   snap.Backend,
					}
				}
			}
		}
	}

	// Backfill / heal under the lock.
	var orphans []string
	healed := 0
	backfilled := 0
	s.mu.Lock()
	for id, ms := range s.sessions {
		if ms.info.ExternalID == "" {
			continue
		}
		val, ok := resolved[snapKey{ms.info.Hostname, ms.info.ExternalID}]
		if !ok {
			// Only flag as orphan if we also can't backfill GitRef — i.e.
			// no backend knows this session at all.
			if ms.info.GitRef.LocalPath == "" && ms.info.GitRef.RemoteURL == "" {
				orphans = append(orphans, id)
			}
			continue
		}
		changed := false
		if val.directory != "" && ms.info.GitRef.LocalPath == "" && ms.info.GitRef.RemoteURL == "" {
			ms.info.GitRef.LocalPath = val.directory
			backfilled++
			changed = true
		}
		if ms.info.Backend != val.backend {
			s.log.Printf("startup-discover: HEAL hub_id=%s backend %s → %s (extID=%s)", id, ms.info.Backend, val.backend, ms.info.ExternalID)
			ms.info.Backend = val.backend
			healed++
			changed = true
		}
		if changed {
			s.persistSession(ms)
		}
	}
	s.mu.Unlock()

	if backfilled > 0 || healed > 0 {
		s.log.Printf("startup-discover: backfilled %d GitRef.LocalPath, healed %d info.Backend", backfilled, healed)
	}

	for _, id := range orphans {
		s.log.Printf("startup-discover: WARN orphan session %s — no backend snapshot matches its ExternalID; left as-is", id)
	}
}
