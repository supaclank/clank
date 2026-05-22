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
// fingerprint, used to skip redundant disk writes when the autoRefresh
// loop polls every 3s while nothing meaningful has happened.
type sessionsCacheSignature struct {
	count  int
	latest int64 // unix ms of the newest UpdatedAt
}

// sessionsCacheSig computes the signature for a session list. Equality
// implies "no row changed and no row was added/removed" — good enough
// for cache-write debouncing.
func sessionsCacheSig(sessions []agent.SessionInfo) sessionsCacheSignature {
	sig := sessionsCacheSignature{count: len(sessions)}
	for _, s := range sessions {
		if ms := s.UpdatedAt.UnixMilli(); ms > sig.latest {
			sig.latest = ms
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
