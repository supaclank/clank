package preview

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/acksell/clank/internal/config"
)

// packagerChoiceEvidence is the ToolEvidence string for installs
// driven by a saved per-project choice rather than repo signals.
const packagerChoiceEvidence = "your saved packager choice"

// packagerChoicePath returns where workDir's saved packager choice
// lives: the clank state dir (never the user's repo — same posture as
// the bootstrap markers), keyed by basename plus a hash of the
// absolute path so same-named folders can't collide. The CLI writes
// it after the one-time prompt; Detect (CLI and daemon alike — same
// machine, same state dir) reads it.
func packagerChoicePath(workDir string) (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", fmt.Errorf("resolve clank dir: %w", err)
	}
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return "", fmt.Errorf("resolve workdir %q: %w", workDir, err)
	}
	sum := sha256.Sum256([]byte(abs))
	name := fmt.Sprintf("%s-%x", filepath.Base(abs), sum[:6])
	return filepath.Join(dir, "preview-packager", name), nil
}

// LoadPackagerChoice returns the saved packager for workDir. ok is
// false when no choice was saved — or when the file's content isn't a
// known Packager (a stale or hand-edited file re-prompts rather than
// driving an undefined install).
func LoadPackagerChoice(workDir string) (Packager, bool) {
	path, err := packagerChoicePath(workDir)
	if err != nil {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	s := strings.TrimSpace(string(data))
	if !isPackagerName(s) {
		return "", false
	}
	return Packager(s), true
}

// SavePackagerChoice persists workDir's packager choice.
func SavePackagerChoice(workDir string, pm Packager) error {
	if !isPackagerName(string(pm)) {
		return fmt.Errorf("unknown packager %q", pm)
	}
	path, err := packagerChoicePath(workDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create choice dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(pm), 0o600); err != nil {
		return fmt.Errorf("save packager choice: %w", err)
	}
	return nil
}
