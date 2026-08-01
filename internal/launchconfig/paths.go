package launchconfig

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/acksell/clank/internal/config"
	internalgit "github.com/acksell/clank/internal/git"
)

const hostLaunchDirectory = "preview-launch"

// ResolvePaths finds the project root and derives its project and host files.
func ResolvePaths(workDir string) (Paths, error) {
	root, isGit, err := findProjectRoot(workDir)
	if err != nil {
		return Paths{}, err
	}
	identity := root
	if isGit {
		identity, err = internalgit.MainWorktreeRoot(root)
		if err != nil {
			return Paths{}, fmt.Errorf("resolve shared git project identity: %w", err)
		}
		identity, err = filepath.Abs(identity)
		if err != nil {
			return Paths{}, fmt.Errorf("resolve shared git project identity %q: %w", identity, err)
		}
	}
	clankDir, err := config.Dir()
	if err != nil {
		return Paths{}, fmt.Errorf("resolve Clank config directory: %w", err)
	}
	clankDir, err = filepath.Abs(clankDir)
	if err != nil {
		return Paths{}, fmt.Errorf("resolve Clank config directory %q: %w", clankDir, err)
	}

	sum := sha256.Sum256([]byte(identity))
	hostFile := fmt.Sprintf("%s-%x.yaml", filepath.Base(identity), sum[:6])
	return Paths{
		ProjectRoot: root,
		Project:     filepath.Join(root, filepath.FromSlash(ProjectRelativePath)),
		Host:        filepath.Join(clankDir, hostLaunchDirectory, hostFile),
	}, nil
}

func findProjectRoot(workDir string) (string, bool, error) {
	if workDir == "" {
		return "", false, fmt.Errorf("launchconfig: work directory is required")
	}
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return "", false, fmt.Errorf("resolve work directory %q: %w", workDir, err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", false, fmt.Errorf("resolve work directory %q: %w", workDir, err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", false, fmt.Errorf("stat work directory %q: %w", workDir, err)
	}
	if !info.IsDir() {
		return "", false, fmt.Errorf("work directory %q is not a directory", workDir)
	}

	for dir := real; ; dir = filepath.Dir(dir) {
		_, err := os.Stat(filepath.Join(dir, ".git"))
		if err == nil {
			return dir, true, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", false, fmt.Errorf("stat git marker in %q: %w", dir, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return real, false, nil
		}
	}
}
