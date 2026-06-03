package sessionsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/pkg/sync/checkpoint"
	syncclient "github.com/acksell/clank/pkg/sync/client"
)

// ExportedSession pairs a complete manifest entry with the temp file
// holding its export blob, for the upload leg. Only CHANGED sessions are
// exported; unchanged sessions ride in the manifest by reference (their
// blob is already in object storage from an earlier push).
type ExportedSession struct {
	Entry    checkpoint.SessionEntry
	BlobPath string
}

// SkippedSession describes a discovered session that was not exported
// (currently: a session whose backend storage has gone missing).
type SkippedSession struct {
	ExternalID string
	Backend    agent.BackendType
	Reason     string
}

// PushPlan is the result of PreparePush: the blobs to upload (changed
// sessions only), the COMPLETE per-checkpoint manifest entries (changed +
// unchanged-by-reference), the new last-pushed record to persist after a
// successful upload, and any skipped orphans. Callers MUST invoke
// Cleanup() to remove the temp blob files.
type PushPlan struct {
	Upload  []ExportedSession
	Entries []checkpoint.SessionEntry
	Record  agent.SyncedSessionRecord
	Skipped []SkippedSession
	tmpDir  string
}

// Cleanup removes the temp blob directory. Safe to call multiple times.
func (p *PushPlan) Cleanup() {
	if p == nil || p.tmpDir == "" {
		return
	}
	_ = os.RemoveAll(p.tmpDir)
	p.tmpDir = ""
}

// PreparePush discovers the worktree's sessions and builds a delta push
// against the last-pushed record rec: it exports + content-hashes ONLY the
// sessions that changed (Unsynced), while assembling a COMPLETE manifest
// (changed sessions plus unchanged ones referenced by their recorded
// content hash) so every checkpoint stays a self-contained snapshot. The
// returned Record is the new last-pushed record to persist after a
// successful upload — rebuilt from the current discovered set, so deleted
// sessions are pruned and unchanged ones keep their address.
//
// Daemon-free: discovery + export read each backend's own storage; no host.
func PreparePush(ctx context.Context, projectDir string, rec agent.SyncedSessionRecord, obs syncclient.PushObserver) (*PushPlan, error) {
	if projectDir == "" {
		return nil, fmt.Errorf("prepare push: projectDir is required")
	}
	all, err := DiscoverWorktreeSessions(ctx, projectDir)
	if err != nil {
		return nil, err
	}
	unsynced := Unsynced(all, rec)

	tmpDir, err := os.MkdirTemp("", "clank-session-export-*")
	if err != nil {
		return nil, fmt.Errorf("prepare push: tempdir: %w", err)
	}
	plan := &PushPlan{
		Record: agent.SyncedSessionRecord{Sessions: make(map[string]agent.SyncedSession, len(all))},
		tmpDir: tmpDir,
	}

	// Export the changed sessions, hashing as we copy. This is the slow
	// local leg, so report it as (i/N) over the CHANGED set.
	type blob struct {
		hash     string
		bytes    int64
		blobPath string
	}
	done := make(map[string]blob, len(unsynced))
	skipped := make(map[string]bool)
	total := len(unsynced)
	if obs != nil {
		obs.SessionProgress(0, total)
	}
	for i, ds := range unsynced {
		blobPath := filepath.Join(tmpDir, ds.ExternalID+blobExt(ds.Backend))
		hash, size, err := exportSessionBlobHashed(ctx, ds, blobPath)
		if err != nil {
			// A missing session means the backend's storage no longer has it
			// (e.g. deleted out of band). Skip the orphan loudly rather than
			// fail the whole push — it's left out of the manifest and record,
			// so a later push retries it.
			if errors.Is(err, ErrSessionNotFound) || isSessionNotFound(err) {
				plan.Skipped = append(plan.Skipped, SkippedSession{
					ExternalID: ds.ExternalID,
					Backend:    ds.Backend,
					Reason:     "backend storage missing this session (deleted out of band?)",
				})
				skipped[ds.ExternalID] = true
				if obs != nil {
					obs.SessionProgress(i+1, total)
				}
				continue
			}
			plan.Cleanup()
			return nil, fmt.Errorf("export session %s: %w", ds.ExternalID, err)
		}
		done[ds.ExternalID] = blob{hash: hash, bytes: size, blobPath: blobPath}
		if obs != nil {
			obs.SessionProgress(i+1, total)
		}
	}

	// Assemble the COMPLETE manifest + the new record from the current set,
	// skipping orphans we couldn't export. Unchanged sessions reuse their
	// recorded content address; changed ones use the fresh hash.
	for _, ds := range all {
		if skipped[ds.ExternalID] {
			continue
		}
		var hash string
		var size int64
		if b, ok := done[ds.ExternalID]; ok {
			hash, size = b.hash, b.bytes
		} else {
			prev := rec.Sessions[ds.ExternalID]
			hash, size = prev.ContentHash, prev.Bytes
		}
		entry := checkpoint.SessionEntry{
			SessionID:   ds.ExternalID, // daemon-free: no separate clank ULID
			ExternalID:  ds.ExternalID,
			Backend:     ds.Backend,
			ContentHash: hash,
			Status:      agent.StatusIdle,
			Title:       ds.Title,
			ProjectDir:  ds.ProjectDir,
			CreatedAt:   ds.CreatedAt,
			UpdatedAt:   ds.UpdatedAt,
			Bytes:       size,
		}
		plan.Entries = append(plan.Entries, entry)
		plan.Record.Sessions[ds.ExternalID] = agent.SyncedSession{
			Backend:     ds.Backend,
			UpdatedAt:   ds.UpdatedAt,
			Fingerprint: ds.Fingerprint,
			ContentHash: hash,
			Bytes:       size,
		}
		if b, ok := done[ds.ExternalID]; ok {
			plan.Upload = append(plan.Upload, ExportedSession{Entry: entry, BlobPath: b.blobPath})
		}
	}

	return plan, nil
}

// exportSessionBlobHashed exports one session to blobPath while computing
// the sha256 of its bytes in a single pass, returning the hex digest and
// byte size.
func exportSessionBlobHashed(ctx context.Context, ds DiscoveredSession, blobPath string) (hash string, size int64, err error) {
	f, err := os.Create(blobPath)
	if err != nil {
		return "", 0, fmt.Errorf("create blob %s: %w", blobPath, err)
	}
	h := sha256.New()
	if err := ExportSessionBlob(ctx, ds.Backend, ds.ProjectDir, ds.ExternalID, io.MultiWriter(f, h)); err != nil {
		_ = f.Close()
		_ = os.Remove(blobPath)
		return "", 0, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(blobPath)
		return "", 0, fmt.Errorf("close blob %s: %w", blobPath, err)
	}
	st, err := os.Stat(blobPath)
	if err != nil {
		return "", 0, fmt.Errorf("stat blob %s: %w", blobPath, err)
	}
	return hex.EncodeToString(h.Sum(nil)), st.Size(), nil
}

// DiscoverWorktreeSessions enumerates the opencode + Claude sessions filed
// under projectDir. opencode discovery is skipped when the binary is absent
// (so a Claude-only machine still exports its Claude sessions — the missing
// binary no longer short-circuits the whole export). Claude discovery reads
// on-disk transcripts via the SDK and is scoped to the exact worktree: the
// SDK expands to sibling git worktrees, but a per-worktree push wants only
// this one, so we filter by samePath.
//
// Shared by PreparePush and `clank status` so both see the same set of
// sessions.
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
		// TODO(ai-review): claudeSessionFingerprint ignores context; the caller's 3s status timeout is not enforced for fingerprint I/O. https://github.com/Acksell/clank/pull/41#discussion_r3343017403
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
