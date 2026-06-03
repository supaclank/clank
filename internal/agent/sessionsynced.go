package agent

// Local record of which sessions this machine last pushed for a worktree,
// stored at $(git rev-parse --absolute-git-dir)/clank/sessions-synced.json
// next to worktree-id (see worktreelocal.go). `clank status` diffs the
// current sessions against it to report unsynced session changes. Purely
// local: a session is only ever edited on the machine it runs on, so
// "synced?" == "unchanged since this machine last pushed it?".

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// syncedSessionsRelPath is the record's path relative to gitDir.
const syncedSessionsRelPath = "clank/sessions-synced.json"

// SyncedSessionsVersion is bumped on a non-backwards-compatible schema
// change; an unknown version reads as "no record" (re-recorded on the next
// push) rather than erroring.
const SyncedSessionsVersion = 2

// SyncedSession is the last-pushed version marker for one session.
// Fingerprint is a content version (Claude: last-message uuid) preferred
// over UpdatedAt when present, so a read-only `claude --resume` (which bumps
// the file mtime but appends no real turn) doesn't read as a change. Empty
// for opencode, whose UpdatedAt is already content-based.
type SyncedSession struct {
	Backend     BackendType `json:"backend"`
	UpdatedAt   time.Time   `json:"updated_at"`
	Fingerprint string      `json:"fingerprint,omitempty"`
	// ContentHash + Bytes are the last-pushed blob's content address and
	// size, so an unchanged session's manifest entry can be rebuilt without
	// re-exporting it (see sessionsync.PreparePush).
	ContentHash string `json:"content_hash,omitempty"`
	Bytes       int64  `json:"bytes,omitempty"`
}

// SyncedSessionRecord is the per-worktree set of last-pushed sessions,
// keyed by backend-native ExternalID.
type SyncedSessionRecord struct {
	Version int `json:"version"`
	// LastPushedAt is when this machine last successfully pushed this
	// worktree (any leg). Surfaced by `clank status` as a recency hint;
	// zero (omitted by status) until the first push records it.
	LastPushedAt time.Time                `json:"last_pushed_at"`
	Sessions     map[string]SyncedSession `json:"sessions"`
}

// ReadSyncedSessions returns the record for projectDir, or a zero record
// (no error) when there's nothing usable: not a git repo, no file yet, a
// corrupt file, or an unknown version. Only genuine IO errors on an
// existing file propagate. Best-effort by design — a missing/garbled
// record must never break `clank status`.
func ReadSyncedSessions(projectDir string) (SyncedSessionRecord, error) {
	zero := SyncedSessionRecord{Sessions: map[string]SyncedSession{}}
	gd, err := GitDir(projectDir)
	if err != nil {
		if errors.Is(err, errNotGitRepo) {
			return zero, nil
		}
		return zero, err
	}
	data, err := os.ReadFile(filepath.Join(gd, syncedSessionsRelPath))
	if os.IsNotExist(err) {
		return zero, nil
	}
	if err != nil {
		return zero, err
	}
	var rec SyncedSessionRecord
	if err := json.Unmarshal(data, &rec); err != nil || rec.Version != SyncedSessionsVersion {
		return zero, nil // corrupt or stale schema → treat as "no record"
	}
	if rec.Sessions == nil {
		rec.Sessions = map[string]SyncedSession{}
	}
	return rec, nil
}

// WriteSyncedSessions persists the record for projectDir at
// <gitDir>/clank/sessions-synced.json, replacing any prior record. Errors
// if projectDir isn't inside a git repo.
func WriteSyncedSessions(projectDir string, rec SyncedSessionRecord) error {
	gd, err := GitDir(projectDir)
	if err != nil {
		return fmt.Errorf("write synced sessions: %w", err)
	}
	rec.Version = SyncedSessionsVersion
	dir := filepath.Join(gd, "clank")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal synced sessions: %w", err)
	}
	return os.WriteFile(filepath.Join(dir, "sessions-synced.json"), append(data, '\n'), 0o644)
}
