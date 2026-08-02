package launchconfig

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/acksell/clank/pkg/preview/tokens"
)

var (
	previewNamePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
	portVariablePattern = regexp.MustCompile(`\$(?:\{` + regexp.QuoteMeta(PortEnvironmentName) + `\}|` +
		regexp.QuoteMeta(PortEnvironmentName) + `(?:[^A-Za-z0-9_]|$))`)
)

// Resolve loads, validates, and selects a launch entry for workDir.
func Resolve(workDir, name string) (*Resolved, error) {
	file, source, paths, err := load(workDir)
	if err != nil {
		return nil, err
	}
	resolvedDirs, names, err := validate(file, paths.ProjectRoot)
	if err != nil {
		return nil, fmt.Errorf("validate %s: %w", source.Path, err)
	}

	selectedName := name
	if selectedName == "" {
		selectedName = file.Default
	}
	preview, ok := file.Previews[selectedName]
	if !ok {
		return nil, fmt.Errorf("preview %q is not defined in %s (available: %s)", selectedName, source.Path, strings.Join(names, ", "))
	}
	return &Resolved{
		Name:        selectedName,
		WorkDir:     resolvedDirs[selectedName],
		Command:     preview.Command,
		Environment: cloneEnvironment(preview.Environment),
		Ready:       preview.Ready,
		Source:      source,
	}, nil
}

func validate(file File, root string) (map[string]string, []string, error) {
	if strings.TrimSpace(file.Default) == "" {
		return nil, nil, fmt.Errorf("default is required")
	}
	if len(file.Previews) == 0 {
		return nil, nil, fmt.Errorf("previews must define at least one entry")
	}
	if _, ok := file.Previews[file.Default]; !ok {
		return nil, nil, fmt.Errorf("default preview %q is not defined", file.Default)
	}

	names := make([]string, 0, len(file.Previews))
	resolvedDirs := make(map[string]string, len(file.Previews))
	for name, preview := range file.Previews {
		if name == tokens.DefaultServiceName {
			return nil, nil, fmt.Errorf("preview name %q is reserved for Expo; choose a descriptive name", name)
		}
		if !previewNamePattern.MatchString(name) {
			return nil, nil, fmt.Errorf("invalid preview name %q", name)
		}
		resolvedDir, err := validatePreview(root, name, preview)
		if err != nil {
			return nil, nil, err
		}
		names = append(names, name)
		resolvedDirs[name] = resolvedDir
	}
	sort.Strings(names)
	return resolvedDirs, names, nil
}

func validatePreview(root, name string, preview Preview) (string, error) {
	if strings.TrimSpace(preview.Directory) == "" {
		return "", fmt.Errorf("preview %q: directory is required", name)
	}
	if !filepath.IsLocal(preview.Directory) {
		return "", fmt.Errorf("preview %q: directory must be a local path within the project", name)
	}
	resolvedDir, err := filepath.EvalSymlinks(filepath.Join(root, preview.Directory))
	if err != nil {
		return "", fmt.Errorf("preview %q: resolve preview directory %q: %w", name, preview.Directory, err)
	}
	if err := requireWithinRoot(root, resolvedDir); err != nil {
		return "", fmt.Errorf("preview %q: directory %q %w", name, preview.Directory, err)
	}
	info, err := os.Stat(resolvedDir)
	if err != nil {
		return "", fmt.Errorf("preview %q: stat preview directory %q: %w", name, preview.Directory, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("preview %q: preview directory %q is not a directory", name, preview.Directory)
	}
	if strings.TrimSpace(preview.Command) == "" {
		return "", fmt.Errorf("preview %q: command is required", name)
	}
	// TODO(ai-review): text match on $PORT doesn't confirm the launched
	// server actually binds it https://github.com/Acksell/clank/pull/209#discussion_r3696030288
	if !portVariablePattern.MatchString(preview.Command) {
		return "", fmt.Errorf("preview %q: command must consume $%s", name, PortEnvironmentName)
	}
	if err := validateEnvironment(preview.Environment); err != nil {
		return "", fmt.Errorf("preview %q: %w", name, err)
	}
	if err := validateReadyPath(preview.Ready.Path); err != nil {
		return "", fmt.Errorf("preview %q: %w", name, err)
	}
	return resolvedDir, nil
}

func requireWithinRoot(root, path string) error {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return fmt.Errorf("cannot be resolved relative to the project root: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("escapes project root")
	}
	return nil
}

func validateReadyPath(path string) error {
	if path == "" {
		return fmt.Errorf("ready.path is required")
	}
	u, err := url.Parse(path)
	if err != nil {
		return fmt.Errorf("ready.path %q is invalid: %w", path, err)
	}
	if u.IsAbs() || u.Host != "" {
		return fmt.Errorf("ready.path must be a path, not an absolute URL")
	}
	if !strings.HasPrefix(u.Path, "/") {
		return fmt.Errorf("ready.path must start with /")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("ready.path must not contain a query or fragment")
	}
	return nil
}
