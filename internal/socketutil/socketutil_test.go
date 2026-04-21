package socketutil

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoveStaleNonExistent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := RemoveStale(filepath.Join(dir, "nope.sock")); err != nil {
		t.Fatalf("expected nil for non-existent path, got %v", err)
	}
}

func TestRemoveStaleSocket(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "real.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln.Close()
	if err := RemoveStale(path); err != nil {
		t.Fatalf("expected nil removing stale socket, got %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("socket should be gone, stat err = %v", err)
	}
}

// Regression: a regular file at the socket path must NOT be removed.
// Without this guard, a bad --socket value would silently delete user data.
func TestRemoveStaleRegularFileRefuses(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "data.txt")
	if err := os.WriteFile(path, []byte("important"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	err := RemoveStale(path)
	if err == nil {
		t.Fatal("expected error refusing to remove regular file")
	}
	if !strings.Contains(err.Error(), "not a unix socket") {
		t.Fatalf("error should mention non-socket: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("regular file must survive, got %v", err)
	}
}
