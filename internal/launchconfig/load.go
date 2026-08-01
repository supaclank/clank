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
		data, err := os.ReadFile(source.Path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return File{}, Source{}, paths, fmt.Errorf("read %s launch config %s: %w", source.Scope, source.Path, err)
		}
		var file File
		if err := yaml.UnmarshalStrict(data, &file); err != nil {
			return File{}, Source{}, paths, fmt.Errorf("parse %s: %w", source.Path, err)
		}
		return file, source, paths, nil
	}
	return File{}, Source{}, paths, &NotFoundError{Paths: paths}
}
