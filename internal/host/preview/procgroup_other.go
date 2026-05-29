//go:build !unix

package preview

import (
	"os/exec"
	"time"
)

// configureProcessGroup is a no-op on non-Unix platforms. The sprite
// runs Linux, so this only matters for Windows developers running
// clank-host locally; they accept the orphan-on-Stop risk.
func configureProcessGroup(_ *exec.Cmd) {}

// stopProcessGroup falls back to whatever the caller already did to
// the parent process — there's no portable group-kill primitive.
// done is consumed so the call still blocks until the wait goroutine
// has reaped the child.
func stopProcessGroup(_ int, done <-chan struct{}, _ time.Duration) {
	<-done
}
