package sessionsync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/pkg/sync/checkpoint"
)

// ExportedSession pairs a manifest entry with the temp file holding its
// opaque export blob. Fingerprint is the discovered content version (local
// only — NOT serialized into the manifest); the caller records it so later
// drift detection is immune to mtime-only changes.
type ExportedSession struct {
	Entry       checkpoint.SessionEntry
	BlobPath    string
	Fingerprint string
}

// SkippedSession describes a discovered session that was not exported
// (currently: opencode sessions whose storage has gone missing).
type SkippedSession struct {
	ExternalID string
	Backend    agent.BackendType
	Reason     string
}

// ExportResult is the output of ExportWorktreeSessions. Callers MUST
// invoke Cleanup() to remove the temp blob files.
type ExportResult struct {
	Exported []ExportedSession
	Skipped  []SkippedSession
	tmpDir   string
}

// Cleanup removes the temp blob directory. Safe to call multiple times.
func (r *ExportResult) Cleanup() {
	if r == nil || r.tmpDir == "" {
		return
	}
	_ = os.RemoveAll(r.tmpDir)
	r.tmpDir = ""
}

// ExportWorktreeSessions discovers the opencode + Claude sessions filed
// under projectDir and exports each to a temp blob, building the manifest
// entries `clank push` uploads. Daemon-free: discovery reads each backend's
// own storage (`opencode session list`, the Claude Agent SDK), export reads
// it directly — no host.db, no server.
//
// SessionEntry.SessionID is the backend-native external id: with no local
// metadata store there is no separate clank ULID, so the backend's own id is
// the cross-machine identity.
func ExportWorktreeSessions(ctx context.Context, projectDir string) (*ExportResult, error) {
	if projectDir == "" {
		return nil, fmt.Errorf("export worktree sessions: projectDir is required")
	}

	sessions, err := DiscoverWorktreeSessions(ctx, projectDir)
	if err != nil {
		return nil, err
	}

	tmpDir, err := os.MkdirTemp("", "clank-session-export-*")
	if err != nil {
		return nil, fmt.Errorf("export sessions: tempdir: %w", err)
	}
	result := &ExportResult{tmpDir: tmpDir}

	for _, ds := range sessions {
		blobPath := filepath.Join(tmpDir, ds.ExternalID+blobExt(ds.Backend))
		f, err := os.Create(blobPath)
		if err != nil {
			result.Cleanup()
			return nil, fmt.Errorf("export sessions: create blob %s: %w", blobPath, err)
		}
		if err := ExportSessionBlob(ctx, ds.Backend, ds.ProjectDir, ds.ExternalID, f); err != nil {
			_ = f.Close()
			_ = os.Remove(blobPath)
			// A missing session means the backend's storage no longer has it
			// (e.g. deleted out of band). Skip the orphan loudly rather than
			// fail the whole push. Mirrors host.ExportSessions.
			if errors.Is(err, ErrSessionNotFound) || isSessionNotFound(err) {
				result.Skipped = append(result.Skipped, SkippedSession{
					ExternalID: ds.ExternalID,
					Backend:    ds.Backend,
					Reason:     "backend storage missing this session (deleted out of band?)",
				})
				continue
			}
			result.Cleanup()
			return nil, fmt.Errorf("export session %s: %w", ds.ExternalID, err)
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

		result.Exported = append(result.Exported, ExportedSession{
			Entry: checkpoint.SessionEntry{
				SessionID:  ds.ExternalID,
				ExternalID: ds.ExternalID,
				Backend:    ds.Backend,
				BlobKey:    blobKeyFor(ds.Backend, ds.ExternalID),
				Status:     agent.StatusIdle,
				Title:      ds.Title,
				ProjectDir: ds.ProjectDir,
				CreatedAt:  ds.CreatedAt,
				UpdatedAt:  ds.UpdatedAt,
				Bytes:      st.Size(),
			},
			BlobPath:    blobPath,
			Fingerprint: ds.Fingerprint,
		})
	}

	return result, nil
}

// DiscoverWorktreeSessions enumerates the opencode + Claude sessions filed
// under projectDir. opencode discovery is skipped when the binary is absent
// (so a Claude-only machine still exports its Claude sessions — the missing
// binary no longer short-circuits the whole export). Claude discovery reads
// on-disk transcripts via the SDK and is scoped to the exact worktree: the
// SDK expands to sibling git worktrees, but a per-worktree push wants only
// this one, so we filter by samePath.
//
// Shared by ExportWorktreeSessions and `clank status` so both see the same
// set of sessions.
func DiscoverWorktreeSessions(ctx context.Context, projectDir string) ([]DiscoveredSession, error) {
	var sessions []DiscoveredSession

	if _, err := exec.LookPath("opencode"); err == nil {
		oc, err := (OpenCodeBackend{}).ListSessions(ctx, projectDir)
		if err != nil {
			return nil, fmt.Errorf("discover opencode sessions: %w", err)
		}
		sessions = append(sessions, oc...)
	}

	claude, err := (ClaudeBackend{}).ListSessions(ctx, projectDir)
	if err != nil {
		return nil, fmt.Errorf("discover claude sessions: %w", err)
	}
	for _, ds := range claude {
		if !samePath(ds.ProjectDir, projectDir) {
			continue
		}
		// Compute the content fingerprint only for the sessions we keep —
		// the SDK expands to every sibling worktree, so fingerprinting
		// before the filter would tail-read transcripts we discard.
		ds.Fingerprint = claudeSessionFingerprint(ds.ProjectDir, ds.ExternalID)
		sessions = append(sessions, ds)
	}

	return sessions, nil
}

// isSessionNotFound matches opencode's "Session not found" error so a
// stale discovery entry skips rather than failing the push. Mirrors the
// same matcher in internal/host.
func isSessionNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Session not found")
}
