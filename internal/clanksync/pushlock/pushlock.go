// Package pushlock provides a per-worktree advisory lock that
// serializes concurrent `clank push` runs (e.g. a Claude Stop hook and
// an opencode idle plugin firing at once) so they don't race the
// checkpoint.
package pushlock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Acquire takes a non-blocking exclusive lock for the worktree whose git
// directory is gitDir, using <gitDir>/clank/autosync.lock. ok is false
// (with a nil error) when another push already holds the lock — the
// caller should exit quietly. Always call release() to unlock when ok.
func Acquire(gitDir string) (ok bool, release func(), err error) {
	dir := filepath.Join(gitDir, "clank")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, nil, fmt.Errorf("pushlock: mkdir %s: %w", dir, err)
	}
	lockPath := filepath.Join(dir, "autosync.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return false, nil, fmt.Errorf("pushlock: open %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return false, nil, nil
		}
		return false, nil, fmt.Errorf("pushlock: flock %s: %w", lockPath, err)
	}
	return true, func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
