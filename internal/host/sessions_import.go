package host

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/pkg/sync/checkpoint"
)

// RegisterImportedSession installs an exported opencode session into
// this host: invokes `opencode import <blobPath>`, then upserts a
// SessionInfo row into host.db keyed by entry.SessionID (the
// cross-machine stable host ULID) with worktreeID stamped onto its
// GitRef. Idempotent — re-running with the same input is safe
// (UpsertSession is upsert, `opencode import` additive-merges by
// message ID, see plan §E and TestOpenCodeImportSemantics).
//
// If opencode allocates a different external_id than the manifest's
// (would contradict TestOpenCodeImportSemantics path (a)), the
// imported external_id wins and a warning is logged.
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
	if entry.Backend != agent.BackendOpenCode {
		return agent.SessionInfo{}, fmt.Errorf("register imported session: backend %q not supported in v1", entry.Backend)
	}

	// Rebase info.directory from the source's local path (e.g.
	// /Users/foo/repo on a laptop) to the destination's
	// workRoot/<worktreeID>. This is solving a REAL cross-machine
	// problem: the source's filesystem path doesn't exist on this
	// host, so opencode files the imported session under a
	// synthetic "global" project unless we rebase. Upstream wants
	// to land a target-directory flag, after which this shim can
	// be removed:
	//   https://github.com/anomalyco/opencode/issues/15797
	//   https://github.com/anomalyco/opencode/pull/15826
	//
	// We deliberately do NOT paper over opencode's import-validator
	// bugs (e.g. requiring strings where the export emits
	// undefined). Those are opencode-side issues — coercing values
	// to satisfy the validator misrepresents data we don't own.
	// If a session fails to import, the error surfaces verbatim;
	// future work in handleSyncSessionsApplyFromURLs can choose to
	// skip-and-warn rather than abort the whole migration.
	rewrittenPath, err := s.rewriteImportBlob(blobPath, worktreeID)
	if err != nil {
		return agent.SessionInfo{}, fmt.Errorf("register imported session %s: rewrite blob: %w", entry.SessionID, err)
	}
	defer os.Remove(rewrittenPath)

	// projectDir on the subprocess is intentionally empty: `opencode
	// import` reads/writes its own storage (HOME-relative) and ignores
	// cwd, and entry.ProjectDir is the SOURCE host's local path —
	// which doesn't exist on a destination host (chdir would fail
	// before opencode even runs). Verified by
	// TestOpenCodeImportSemantics.
	importedExternalID, err := agent.OpenCodeImportSession(ctx, "", rewrittenPath)
	if err != nil {
		return agent.SessionInfo{}, fmt.Errorf("register imported session %s: %w", entry.SessionID, err)
	}
	if entry.ExternalID != "" && importedExternalID != entry.ExternalID {
		s.log.Printf("register imported session %s: opencode allocated fresh external_id %q (manifest had %q); using imported", entry.SessionID, importedExternalID, entry.ExternalID)
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

// rewriteImportBlob reads an opencode export blob, rebases
// info.directory to the destination's local worktree path, and
// writes the result to a sibling temp file. Returns the new path.
// Caller owns cleanup (os.Remove).
//
// This is the ONLY blob mutation we perform. The rebase exists
// because info.directory in the source's blob is a filesystem path
// that doesn't resolve on the destination — opencode would file the
// imported session under a synthetic "global" project without it.
// We deliberately do not coerce or fabricate any other field; any
// validation errors from `opencode import` are opencode-side bugs
// that should be surfaced, not papered over.
//
// Every field not explicitly touched is preserved verbatim. JSON
// parsing uses map[string]any so we don't have to track opencode's
// full schema.
func (s *Service) rewriteImportBlob(srcPath, worktreeID string) (string, error) {
	destDir, err := workRootDir()
	if err != nil {
		return "", fmt.Errorf("resolve work root: %w", err)
	}
	destDir = filepath.Join(destDir, worktreeID)

	raw, err := os.ReadFile(srcPath)
	if err != nil {
		return "", fmt.Errorf("read blob: %w", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("parse blob: %w", err)
	}
	info, ok := doc["info"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("blob missing top-level info object")
	}
	info["directory"] = destDir
	// Force opencode to rederive projectID from the new directory.
	// Leaving the source's value would peg the imported session to a
	// project hash that doesn't exist on this host, defeating the
	// rewrite. Empty string is honored as "rederive" — verified via
	// TestRegisterImportedSession_RewritesDirectoryToDestination.
	info["projectID"] = ""
	doc["info"] = info

	out, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("marshal rewritten blob: %w", err)
	}
	dstPath := srcPath + ".rewritten.json"
	if err := os.WriteFile(dstPath, out, 0o600); err != nil {
		return "", fmt.Errorf("write rewritten blob: %w", err)
	}
	return dstPath, nil
}
