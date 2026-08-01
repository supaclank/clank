package launchconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ResolvePaths finds the project root and derives its launch file.
func ResolvePaths(workDir string) (Paths, error) {
	root, err := findProjectRoot(workDir)
	if err != nil {
		return Paths{}, err
	}
	return Paths{
		ProjectRoot: root,
		Project:     filepath.Join(root, filepath.FromSlash(ProjectRelativePath)),
	}, nil
}

func findProjectRoot(workDir string) (string, error) {
	if workDir == "" {
		return "", fmt.Errorf("launchconfig: work directory is required")
	}
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return "", fmt.Errorf("resolve work directory %q: %w", workDir, err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve work directory %q: %w", workDir, err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("stat work directory %q: %w", workDir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("work directory %q is not a directory", workDir)
	}

	for dir := real; ; dir = filepath.Dir(dir) {
		_, err := os.Stat(filepath.Join(dir, ".git"))
		if err == nil {
			return dir, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("stat git marker in %q: %w", dir, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return real, nil
		}
	}
}
