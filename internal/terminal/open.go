package terminal

import (
	"fmt"
	"os/exec"
)

// OpenSession opens an OpenCode session in a new terminal window.
// Currently supports Ghostty; falls back to printing the command.
func OpenSession(sessionID string) error {
	if path, err := exec.LookPath("ghostty"); err == nil {
		cmd := exec.Command(path, "-e", "opencode", "-s", sessionID)
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("start ghostty: %w", err)
		}
		// Detach: don't wait for the process.
		go cmd.Wait()
		return nil
	}

	// Fallback for unsupported terminals.
	return fmt.Errorf("no supported terminal found. Run manually: opencode -s %s", sessionID)
}
