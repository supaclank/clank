package launchconfig

import (
	"strings"
	"testing"
)

func TestSetupPromptIncludesContractAndStorageChoice(t *testing.T) {
	t.Parallel()

	paths := Paths{
		ProjectRoot: "/work/project",
		Project:     "/work/project/.clank/launch.yaml",
		Host:        "/host/preview-launch/project.yaml",
	}
	prompt := SetupPrompt(paths)
	for _, required := range []string{
		paths.ProjectRoot,
		paths.Project,
		paths.Host,
		`command: npm run dev -- --host 127.0.0.1 --port "$PORT"`,
		"Do not use Docker or Podman",
		"bind the development server to 127.0.0.1",
		"immutable/frozen install mode",
		"Do not commit, push, or open a pull request unless the user explicitly asks",
	} {
		if !strings.Contains(prompt, required) {
			t.Errorf("SetupPrompt missing %q", required)
		}
	}
}
