package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/pkg/sync/checkpoint"
)

// SessionExportResult is the output of Service.ExportSessions. Each
// SessionEntry is paired with a temp file on disk holding the
// opaque opencode export blob. The caller MUST invoke Cleanup() to
// remove the temp files.
//
// Skipped lists sessions that were enumerated but not exported
// (currently: non-opencode backends in v1 — see plan §G). They are
// surfaced so the CLI can warn the user.
type SessionExportResult struct {
	Entries   []checkpoint.SessionEntry
	BlobPaths map[string]string // sessionID -> temp file path
	Skipped   []SkippedSession
}

// SkippedSession describes a session that was enumerated by
// ExportSessions but not included in the manifest. Currently used
// for claude-code sessions in v1.
type SkippedSession struct {
	SessionID string
	Backend   agent.BackendType
	Reason    string
}

// Cleanup removes the temp blob files. Safe to call multiple times.
func (r *SessionExportResult) Cleanup() {
	if r == nil {
		return
	}
	for _, p := range r.BlobPaths {
		_ = os.Remove(p)
	}
	r.BlobPaths = nil
}

// ExportSessions enumerates the worktree's sessions, quiesces any
// that are busy (immediate abort — no idle wait), and exports each
// opencode session via `opencode export`. Returns a
// SessionExportResult that pairs each manifest entry with a temp
// file holding the opaque export blob.
//
// Claude-code sessions are skipped with a warning in v1 (see plan
// §G); they appear in result.Skipped so the CLI can surface them.
//
// checkpointID is stamped into each SessionEntry.BlobKey for clarity
// — the actual S3 key is constructed by the sync server via
// storage.KeyForSession at presign time.
//
// createdBy is recorded on the SessionManifest by the caller; this
// function only fills the per-session entries.
func (s *Service) ExportSessions(ctx context.Context, worktreeID, checkpointID string) (*SessionExportResult, error) {
	if s.sessionsStore == nil {
		return nil, fmt.Errorf("export sessions: sessions store not configured")
	}
	if worktreeID == "" {
		return nil, fmt.Errorf("export sessions: worktreeID is required")
	}
	if checkpointID == "" {
		return nil, fmt.Errorf("export sessions: checkpointID is required")
	}

	sessions, err := s.sessionsStore.ListSessionsByWorktree(ctx, worktreeID)
	if err != nil {
		return nil, fmt.Errorf("export sessions: list by worktree %s: %w", worktreeID, err)
	}

	result := &SessionExportResult{
		BlobPaths: make(map[string]string, len(sessions)),
	}

	tmpDir, err := os.MkdirTemp("", "clank-session-export-*")
	if err != nil {
		return nil, fmt.Errorf("export sessions: tempdir: %w", err)
	}

	for _, info := range sessions {
		if info.Backend != agent.BackendOpenCode {
			s.log.Printf("export sessions: skipping %s (backend %q not supported in v1)", info.ID, info.Backend)
			result.Skipped = append(result.Skipped, SkippedSession{
				SessionID: info.ID,
				Backend:   info.Backend,
				Reason:    "claude-code session sync not yet implemented",
			})
			continue
		}

		wasBusy := info.Status == agent.StatusBusy
		if wasBusy {
			s.log.Printf("export sessions: aborting busy session %s for migration", info.ID)
			if err := s.AbortSession(ctx, info.ID); err != nil {
				// Abort is best-effort. Log and proceed — the export
				// will read whatever state is on disk; if a write was
				// truly in flight, the worst case is a stale message
				// that gets cleaned up on re-import (additive merge).
				s.log.Printf("export sessions: abort %s: %v (proceeding)", info.ID, err)
			}
		}

		blobPath := filepath.Join(tmpDir, info.ID+".json")
		f, err := os.Create(blobPath)
		if err != nil {
			result.Cleanup()
			return nil, fmt.Errorf("export sessions: create blob file %s: %w", blobPath, err)
		}
		// projectDir is intentionally empty for the same reason
		// RegisterImportedSession doesn't pass it: `opencode export`
		// reads its own storage HOME-relative and ignores cwd, and
		// info.GitRef.LocalPath can hold a stale or destination-
		// invalid path (e.g. the laptop's filesystem path baked
		// into a previously-imported session). chdir into a
		// non-existent path fails before opencode even runs —
		// reproduced on pull --migrate when the sprite tries to
		// export sessions it had imported from a laptop. Pinned by
		// TestExportSessions_IgnoresStaleLocalPath.
		if err := agent.OpenCodeExportSession(ctx, "", info.ExternalID, f); err != nil {
			_ = f.Close()
			_ = os.Remove(blobPath)
			// "Session not found" means the host.db row has gone
			// stale relative to opencode's storage — typically
			// because the user (or some upstream cleanup) ran
			// `opencode session delete`. Skip the orphan with a
			// loud log line; one bad row must not fail the whole
			// migration. Future work could optionally self-heal
			// by deleting the host.db row, but for now we leave
			// it alone so the user can see what was lost. Pinned
			// by TestExportSessions_SkipsMissingOpencodeSession.
			if isSessionNotFound(err) {
				s.log.Printf("export sessions: skipping %s (external_id=%q): opencode reports session not found — host.db row is orphaned", info.ID, info.ExternalID)
				result.Skipped = append(result.Skipped, SkippedSession{
					SessionID: info.ID,
					Backend:   info.Backend,
					Reason:    "opencode storage missing this session (host.db orphan; was it deleted via opencode CLI?)",
				})
				continue
			}
			result.Cleanup()
			return nil, fmt.Errorf("export sessions: %s: %w", info.ID, err)
		}
		if err := f.Close(); err != nil {
			result.Cleanup()
			return nil, fmt.Errorf("export sessions: close blob %s: %w", blobPath, err)
		}

		st, err := os.Stat(blobPath)
		if err != nil {
			result.Cleanup()
			return nil, fmt.Errorf("export sessions: stat blob %s: %w", blobPath, err)
		}

		entry := checkpoint.SessionEntry{
			SessionID:      info.ID,
			ExternalID:     info.ExternalID,
			Backend:        info.Backend,
			BlobKey:        "sessions/" + info.ID + ".json",
			Status:         info.Status,
			Title:          info.Title,
			Prompt:         info.Prompt,
			TicketID:       info.TicketID,
			Agent:          info.Agent,
			WorktreeBranch: info.GitRef.WorktreeBranch,
			ProjectDir:     info.GitRef.LocalPath,
			CreatedAt:      info.CreatedAt,
			UpdatedAt:      info.UpdatedAt,
			Bytes:          st.Size(),
			WasBusy:        wasBusy,
		}
		result.Entries = append(result.Entries, entry)
		result.BlobPaths[info.ID] = blobPath
	}

	return result, nil
}

// isSessionNotFound returns true when err looks like opencode's
// "Session not found" response. We match on the substring rather
// than a typed error because OpenCodeExportSession wraps stderr
// from a subprocess; opencode's CLI doesn't expose error codes
// the Go side can switch on.
//
// The literal string is from opencode 1.x — "Session not found:
// ses_…". If a future opencode version changes the wording this
// goes back to surfacing as a hard error, which is a survivable
// regression (we'll see the new message and update this matcher).
func isSessionNotFound(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "Session not found")
}
