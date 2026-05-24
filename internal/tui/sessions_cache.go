package tui

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/config"
)

// sessionsCacheFilename is the on-disk file under config.Dir() that
// holds the last successful session list from the daemon. Re-read on
// startup so the sidebar can render before the (~0.5–1s) daemon round
// trip completes — fresh data overwrites the cached frame as soon as
// the live load returns.
const sessionsCacheFilename = "sessions-cache.json"

// sessionsCacheSignature is a cheap "did the data materially change?"
// fingerprint, used to skip redundant disk writes when SSE event
// bursts (status churn from a busy backend) would otherwise hammer
// the cache file.
type sessionsCacheSignature struct {
	count        int
	latestUpdate int64 // unix ms of newest UpdatedAt
	latestRead   int64 // unix ms of newest LastReadAt
}

// sessionsCacheSig computes the signature for a session list. Equality
// implies "no row changed and no row was added/removed" — good enough
// for cache-write debouncing. LastReadAt is tracked separately so a
// MarkRead (which doesn't bump UpdatedAt) still invalidates the sig
// and triggers a write — otherwise the disk cache misses read-state
// updates that arrive purely via SSE.
func sessionsCacheSig(sessions []agent.SessionInfo) sessionsCacheSignature {
	sig := sessionsCacheSignature{count: len(sessions)}
	for _, s := range sessions {
		if ms := s.UpdatedAt.UnixMilli(); ms > sig.latestUpdate {
			sig.latestUpdate = ms
		}
		if ms := s.LastReadAt.UnixMilli(); ms > sig.latestRead {
			sig.latestRead = ms
		}
	}
	return sig
}

func sessionsCachePath() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, sessionsCacheFilename), nil
}

// loadSessionsCache returns the most recently saved session list, or
// (nil, err) when no cache exists / the file is corrupt. Callers treat
// a nil result as "no cached frame yet — wait for the live load."
func loadSessionsCache() ([]agent.SessionInfo, error) {
	path, err := sessionsCachePath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sessions []agent.SessionInfo
	if err := json.Unmarshal(data, &sessions); err != nil {
		return nil, err
	}
	return sessions, nil
}

// saveSessionsCache atomically writes the session list to disk. Uses
// the standard tmp-file + rename pattern so a crashed write can't
// leave the cache half-formed (which would then fail to JSON-decode
// on next startup and silently fall back to "no cache").
func saveSessionsCache(sessions []agent.SessionInfo) error {
	path, err := sessionsCachePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(sessions)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
