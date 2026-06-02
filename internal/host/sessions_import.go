package host

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/pkg/sessionsync"
	"github.com/acksell/clank/pkg/sync/checkpoint"
)

// RegisterImportedSession installs an exported session into this host via
// sessionsync.ImportSessionBlob (routed by backend), then upserts a
// SessionInfo row into host.db keyed by entry.SessionID (the
// cross-machine stable host ULID) with worktreeID stamped onto its
// GitRef. Idempotent — re-running with the same input is safe
// (UpsertSession is upsert; opencode import additive-merges by message
// ID, Claude overwrites with the superset transcript).
//
// If the backend allocates a different external_id than the manifest's,
// the imported external_id wins and a warning is logged.
func (s *Service) RegisterImportedSession(ctx context.Context, worktreeID string, entry checkpoint.SessionEntry, blobPath string) (agent.SessionInfo, error) {
	if s.sessionsStore == nil {
		return agent.SessionInfo{}, fmt.Errorf("register imported session: sessions store not configured")
	}
	if worktreeID == "" {
		return agent.SessionInfo{}, fmt.Errorf("register imported session: worktreeID is required")
	}
	if entry.SessionID == "" {
		return agent.SessionInfo{}, fmt.Errorf("register imported session: entry.SessionID is required")
	}
	if entry.Backend == "" {
		return agent.SessionInfo{}, fmt.Errorf("register imported session: entry.Backend is required")
	}

	destDir, err := workRootDir()
	if err != nil {
		return agent.SessionInfo{}, fmt.Errorf("register imported session %s: resolve work root: %w", entry.SessionID, err)
	}

	importedExternalID, err := sessionsync.ImportSessionBlob(ctx, entry.Backend, blobPath, filepath.Join(destDir, worktreeID))
	if err != nil {
		return agent.SessionInfo{}, fmt.Errorf("register imported session %s: %w", entry.SessionID, err)
	}
	if entry.ExternalID != "" && importedExternalID != entry.ExternalID {
		s.log.Printf("register imported session %s: backend %s allocated fresh external_id %q (manifest had %q); using imported", entry.SessionID, entry.Backend, importedExternalID, entry.ExternalID)
	}

	now := time.Now()
	// Status on a freshly-imported session is always idle — the abort
	// happened on the source, and opencode import doesn't preserve
	// in-flight tool calls. The manifest's WasBusy flag stays as a
	// hint for the (future) auto-resume feature.
	info := agent.SessionInfo{
		ID:         entry.SessionID,
		ExternalID: importedExternalID,
		Backend:    entry.Backend,
		Status:     agent.StatusIdle,
		Hostname:   s.id,
		GitRef: agent.GitRef{
			WorktreeID:     worktreeID,
			WorktreeBranch: entry.WorktreeBranch,
		},
		Prompt:    entry.Prompt,
		Title:     entry.Title,
		TicketID:  entry.TicketID,
		Agent:     entry.Agent,
		CreatedAt: nonZeroOr(entry.CreatedAt, now),
		UpdatedAt: now,
	}

	if err := s.sessionsStore.UpsertSession(ctx, info); err != nil {
		return agent.SessionInfo{}, fmt.Errorf("register imported session %s: upsert: %w", entry.SessionID, err)
	}
	return info, nil
}

func nonZeroOr(t, fallback time.Time) time.Time {
	if t.IsZero() {
		return fallback
	}
	return t
}
