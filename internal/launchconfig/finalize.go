package launchconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// FinalizeSetup validates the agent output and installs a host-only candidate.
// Shared project output is validated in place.
func FinalizeSetup(workDir string, scope Scope) (*Resolved, error) {
	paths, err := ResolvePaths(workDir)
	if err != nil {
		return nil, err
	}
	outputPath, err := SetupOutputPath(paths, scope)
	if err != nil {
		return nil, err
	}
	file, data, err := readLaunchFile(outputPath)
	if err != nil {
		return nil, fmt.Errorf("validate generated launch config: %w", err)
	}
	if _, _, err := validate(file, paths.ProjectRoot); err != nil {
		return nil, fmt.Errorf("validate generated launch config %s: %w", outputPath, err)
	}

	if scope == ScopeHost {
		if err := installHostConfig(paths.Host, data); err != nil {
			return nil, err
		}
		if err := os.Remove(outputPath); err != nil {
			return nil, fmt.Errorf("remove validated setup candidate %s: %w", outputPath, err)
		}
		if err := os.Remove(filepath.Dir(outputPath)); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("remove empty setup directory %s: %w", filepath.Dir(outputPath), err)
		}
	}

	resolved, err := Resolve(workDir, "")
	if err != nil {
		return nil, fmt.Errorf("resolve generated launch config: %w", err)
	}
	return resolved, nil
}

func installHostConfig(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create host launch directory %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".launch-*.yaml")
	if err != nil {
		return fmt.Errorf("create temporary host launch config: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = tmp.Close() }()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("set temporary host launch config permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temporary host launch config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temporary host launch config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary host launch config: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("install host launch config %s: %w", path, err)
	}
	return nil
}
