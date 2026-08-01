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

	file, _, err := readLaunchFile(paths.Project)
	if errors.Is(err, os.ErrNotExist) {
		return File{}, Source{}, paths, &NotFoundError{Paths: paths}
	}
	if err != nil {
		return File{}, Source{}, paths, fmt.Errorf("load project launch config: %w", err)
	}
	return file, Source{Path: paths.Project}, paths, nil
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
