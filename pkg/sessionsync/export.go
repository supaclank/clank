package sessionsync

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/pkg/sync/checkpoint"
)

// ExportedSession pairs a manifest entry with the temp file holding its
// opaque export blob.
type ExportedSession struct {
	Entry    checkpoint.SessionEntry
	BlobPath string
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

// ExportWorktreeSessions discovers the opencode sessions filed under
// projectDir and exports each to a temp blob, building the manifest
// entries `clank push` uploads. Daemon-free: discovery uses `opencode
// session list`, export uses `opencode export` — no host.db, no server.
//
// SessionEntry.SessionID is the opencode external id: with no local
// metadata store there is no separate clank ULID, so the backend-native
// id is the cross-machine identity. Claude sessions are not exported
// (transfer is not implemented).
func ExportWorktreeSessions(ctx context.Context, projectDir string) (*ExportResult, error) {
	if projectDir == "" {
		return nil, fmt.Errorf("export worktree sessions: projectDir is required")
	}

	// No opencode on this machine → no opencode sessions to export. Let
	// the code push proceed rather than failing on a missing binary.
	if _, err := exec.LookPath("opencode"); err != nil {
		return &ExportResult{}, nil
	}

	oc := OpenCodeBackend{}
	sessions, err := oc.ListSessions(ctx, projectDir)
	if err != nil {
		return nil, fmt.Errorf("discover opencode sessions: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "clank-session-export-*")
	if err != nil {
		return nil, fmt.Errorf("export sessions: tempdir: %w", err)
	}
	result := &ExportResult{tmpDir: tmpDir}

	for _, ds := range sessions {
		blobPath := filepath.Join(tmpDir, ds.ExternalID+".json")
		f, err := os.Create(blobPath)
		if err != nil {
			result.Cleanup()
			return nil, fmt.Errorf("export sessions: create blob %s: %w", blobPath, err)
		}
		if err := oc.ExportSession(ctx, "", ds.ExternalID, f); err != nil {
			_ = f.Close()
			_ = os.Remove(blobPath)
			// A missing session means opencode's storage no longer has it
			// (e.g. deleted via the CLI). Skip the orphan loudly rather
			// than fail the whole push. Mirrors host.ExportSessions.
			if isSessionNotFound(err) {
				result.Skipped = append(result.Skipped, SkippedSession{
					ExternalID: ds.ExternalID,
					Backend:    agent.BackendOpenCode,
					Reason:     "opencode storage missing this session (deleted via CLI?)",
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
				Backend:    agent.BackendOpenCode,
				BlobKey:    "sessions/" + ds.ExternalID + ".json",
				Status:     agent.StatusIdle,
				Title:      ds.Title,
				ProjectDir: ds.ProjectDir,
				CreatedAt:  ds.CreatedAt,
				UpdatedAt:  ds.UpdatedAt,
				Bytes:      st.Size(),
			},
			BlobPath: blobPath,
		})
	}

	return result, nil
}

// isSessionNotFound matches opencode's "Session not found" error so a
// stale discovery entry skips rather than failing the push. Mirrors the
// same matcher in internal/host.
func isSessionNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Session not found")
}
