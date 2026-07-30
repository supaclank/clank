package clankyaml

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Load reads and validates dir's clank.yaml.
//
//   - (nil, nil): no clank.yaml — a normal answer, callers fall back
//     to auto-detection.
//   - (nil, err): the file exists but is unreadable, malformed, or
//     invalid. The user wrote config; a broken file must surface
//     loudly, never degrade to "not previewable".
//   - (*File, nil): parsed and validated.
func Load(dir string) (*File, error) {
	path := filepath.Join(dir, FileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	f, err := parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// parse strict-decodes data into a File and validates it. Split from
// Load so table tests can feed literals without touching the disk.
func parse(data []byte) (*File, error) {
	var f File
	dec := yaml.NewDecoder(bytes.NewReader(data))
	// Unknown keys inside known sections are errors; unknown top-level
	// sections land in File.Rest and pass.
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) {
			// Empty file: treat as "no config", same as an absent one.
			return nil, nil
		}
		return nil, fmt.Errorf("parse: %w", err)
	}
	// A trailing `---`-delimited document must not silently vanish —
	// decode it too so a malformed or unexpected second document errors
	// instead of being ignored.
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("parse: clank.yaml must contain a single YAML document")
		}
		return nil, fmt.Errorf("parse: %w", err)
	}
	if err := f.validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

func (f *File) validate() error {
	p := f.Preview
	if p == nil {
		return nil
	}
	if p.Dir != "" && !filepath.IsLocal(p.Dir) {
		return fmt.Errorf("preview.dir %q must be a relative path inside the repo", p.Dir)
	}
	if p.Command != "" && !strings.Contains(p.Command, PortPlaceholder) {
		return fmt.Errorf("preview.command must contain %s — clank allocates the port and substitutes it (a command without the placeholder would listen where the preview proxy never looks)", PortPlaceholder)
	}
	if p.Ready != nil {
		if p.Ready.Path == "" {
			return fmt.Errorf("preview.ready.path is required when a ready block is present")
		}
		if !strings.HasPrefix(p.Ready.Path, "/") {
			return fmt.Errorf("preview.ready.path %q must start with /", p.Ready.Path)
		}
	}
	return nil
}
