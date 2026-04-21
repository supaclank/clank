// Package socketutil provides safe filesystem operations for Unix domain
// sockets shared across the clank-host and clankd binaries.
package socketutil

import (
	"fmt"
	"os"
)

// RemoveStale unlinks path only if it exists AND is a Unix domain socket.
// Returns nil if path does not exist. Returns an error if path exists but
// is some other kind of file (regular file, directory, symlink, etc.) so
// callers cannot accidentally clobber user data via a bad --socket value
// or a hand-replaced file.
func RemoveStale(path string) error {
	fi, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a unix socket", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}
