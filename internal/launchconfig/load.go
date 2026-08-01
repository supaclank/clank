package launchconfig

import (
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v2"
)

func load(workDir string) (File, Source, Paths, error) {
	paths, err := ResolvePaths(workDir)
	if err != nil {
		return File{}, Source{}, Paths{}, err
	}

	sources := []Source{
		{Scope: ScopeProject, Path: paths.Project},
		{Scope: ScopeHost, Path: paths.Host},
	}
	for _, source := range sources {
		file, _, err := readLaunchFile(source.Path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return File{}, Source{}, paths, fmt.Errorf("load %s launch config: %w", source.Scope, err)
		}
		return file, source, paths, nil
	}
	return File{}, Source{}, paths, &NotFoundError{Paths: paths}
}

func readLaunchFile(path string) (File, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return File{}, nil, fmt.Errorf("read %s: %w", path, err)
	}
	var file File
	if err := yaml.UnmarshalStrict(data, &file); err != nil {
		return File{}, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return file, data, nil
}
