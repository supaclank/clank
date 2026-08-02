// Package launchconfig loads Clank's project-specific web preview launch files.
package launchconfig

const (
	// ProjectRelativePath is the repository-owned launch configuration path.
	ProjectRelativePath = ".clank/launch.yaml"
	// PortEnvironmentName is the allocated-port contract for every command.
	PortEnvironmentName = "PORT"
	// PublicHostnameEnvironmentName is the hostname clients use to reach a preview.
	PublicHostnameEnvironmentName = "CLANK_PREVIEW_PUBLIC_HOSTNAME"
)

// File is the strict YAML launch schema.
type File struct {
	Default  string             `yaml:"default"`
	Previews map[string]Preview `yaml:"previews"`
}

// Preview declares one named web development server.
type Preview struct {
	Directory   string            `yaml:"directory"`
	Command     string            `yaml:"command"`
	Environment map[string]string `yaml:"env"`
	Ready       Ready             `yaml:"ready"`
}

// Ready declares the HTTP response that marks a server ready.
type Ready struct {
	Path              string `yaml:"path"`
	ExpectedSubstring string `yaml:"expect"`
}

// Source identifies the selected project configuration file.
type Source struct {
	Path string
}

// Paths contains the project identity and launch configuration location.
type Paths struct {
	ProjectRoot string
	Project     string
}

// Resolved is one validated launch entry with an absolute working directory.
type Resolved struct {
	Name        string
	WorkDir     string
	Command     string
	Environment map[string]string
	Ready       Ready
	Source      Source
}

// NotFoundError carries the paths offered by one-time setup.
type NotFoundError struct {
	Paths Paths
}

func (e *NotFoundError) Error() string {
	return "no Clank preview launch configuration exists for this project"
}
