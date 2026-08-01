// Package launchconfig loads Clank's project-specific web preview launch files.
package launchconfig

const (
	// ProjectRelativePath is the repository-owned launch configuration path.
	ProjectRelativePath = ".clank/launch.yaml"
	// SetupRelativePath is the project-local candidate used before Clank
	// installs a validated host-only configuration.
	SetupRelativePath = ".clank/launch.setup.yaml"
	// PortEnvironmentName is the allocated-port contract for every command.
	PortEnvironmentName = "PORT"
)

// Scope identifies whether configuration is shared with the project or host-only.
type Scope string

const (
	// ScopeProject selects the repository-owned file.
	ScopeProject Scope = "project"
	// ScopeHost selects the persistent host-only file.
	ScopeHost Scope = "host"
)

// File is the strict YAML launch schema.
type File struct {
	Default  string             `yaml:"default"`
	Previews map[string]Preview `yaml:"previews"`
}

// Preview declares one named web development server.
type Preview struct {
	Directory string `yaml:"directory"`
	Command   string `yaml:"command"`
	Ready     Ready  `yaml:"ready"`
}

// Ready declares the HTTP response that marks a server ready.
type Ready struct {
	Path              string `yaml:"path"`
	ExpectedSubstring string `yaml:"expect"`
}

// Source identifies the selected project or host configuration file.
type Source struct {
	Scope Scope
	Path  string
}

// Paths contains the project identity and both supported config locations.
type Paths struct {
	ProjectRoot string
	Project     string
	Host        string
}

// Resolved is one validated launch entry with an absolute working directory.
type Resolved struct {
	Name    string
	WorkDir string
	Command string
	Ready   Ready
	Source  Source
}

// NotFoundError carries the paths offered by one-time setup.
type NotFoundError struct {
	Paths Paths
}

func (e *NotFoundError) Error() string {
	return "no Clank preview launch configuration exists for this project"
}
