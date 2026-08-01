package launchconfig

import (
	"fmt"
)

// FinalizeSetup validates the generated project configuration in place.
func FinalizeSetup(workDir string) (*Resolved, error) {
	paths, err := ResolvePaths(workDir)
	if err != nil {
		return nil, err
	}
	file, _, err := readLaunchFile(paths.Project)
	if err != nil {
		return nil, fmt.Errorf("validate generated launch config: %w", err)
	}
	if _, _, err := validate(file, paths.ProjectRoot); err != nil {
		return nil, fmt.Errorf("validate generated launch config %s: %w", paths.Project, err)
	}

	resolved, err := Resolve(workDir, "")
	if err != nil {
		return nil, fmt.Errorf("resolve generated launch config: %w", err)
	}
	return resolved, nil
}
