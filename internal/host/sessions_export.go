package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/pkg/sessionsync"
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
	BlobPaths map[string]string // externalID -> temp file path
	Skipped   []SkippedSession
}

// SkippedSession describes a session that was enumerated by
// ExportSessions but not included in the manifest — currently only
// orphans whose backend storage has gone missing (host.db row stale).
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
// session via its backend (sessionsync.ExportSessionBlob). Returns a
// SessionExportResult that pairs each manifest entry with a temp
// file holding the opaque export blob.
//
// Orphans whose backend storage has gone missing appear in
// result.Skipped so the CLI can surface them.
//
// checkpointID gates the build; the session blob's S3 key is
// content-addressed (storage.KeyForSessionBlob) from the entry's
// ExternalID + ContentHash at presign time.
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

	workRoot, err := workRootDir()
	if err != nil {
		return nil, fmt.Errorf("export sessions: resolve work root: %w", err)
	}

	for _, info := range sessions {
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
		// cwd matters only for Claude (it files transcripts by encoded
		// directory); opencode ignores it and exports HOME-relative. We
		// pass the worktree path computed fresh from workRoot — NOT
		// info.GitRef.LocalPath, which can be a stale source-host path
		// baked into a previously-imported session (chdir into a
		// non-existent path fails before the backend runs). Pinned by
		// TestExportSessions_IgnoresStaleLocalPath.
		cwd := filepath.Join(workRoot, worktreeID)
		h := sha256.New()
		if err := sessionsync.ExportSessionBlob(ctx, info.Backend, cwd, info.ExternalID, io.MultiWriter(f, h)); err != nil {
			_ = f.Close()
			_ = os.Remove(blobPath)
			// A missing session means the host.db row has gone stale
			// relative to the backend's storage — typically because the
			// session was deleted via the backend CLI. Skip the orphan
			// with a loud log line; one bad row must not fail the whole
			// export. Pinned by TestExportSessions_SkipsMissingOpencodeSession.
			if errors.Is(err, sessionsync.ErrSessionNotFound) || isSessionNotFound(err) {
				s.log.Printf("export sessions: skipping %s (external_id=%q, backend=%s): backend reports session not found — host.db row is orphaned", info.ID, info.ExternalID, info.Backend)
				result.Skipped = append(result.Skipped, SkippedSession{
					SessionID: info.ID,
					Backend:   info.Backend,
					Reason:    "backend storage missing this session (host.db orphan; deleted via backend CLI?)",
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
			ContentHash:    hex.EncodeToString(h.Sum(nil)),
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
		result.BlobPaths[info.ExternalID] = blobPath
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
