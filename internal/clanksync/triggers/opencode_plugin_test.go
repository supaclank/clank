package triggers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallOpenCodePlugin(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if err := InstallOpenCodePlugin("/abs/clank", dir); err != nil {
		t.Fatalf("install: %v", err)
	}
	path := filepath.Join(dir, "plugins", pluginFileName)
	content := readFile(t, path)
	if !strings.Contains(content, `"/abs/clank"`) {
		t.Errorf("clank path not injected as a quoted literal:\n%s", content)
	}
	if !strings.Contains(content, "session.idle") {
		t.Error("plugin missing session.idle handler")
	}

	// Idempotent overwrite updates the path.
	if err := InstallOpenCodePlugin("/abs/clank2", dir); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(t, path), `"/abs/clank2"`) {
		t.Error("re-install didn't update the binary path")
	}

	// Uninstall removes the file; second uninstall is a no-op.
	if err := UninstallOpenCodePlugin(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("plugin not removed by uninstall: %v", err)
	}
	if err := UninstallOpenCodePlugin(dir); err != nil {
		t.Errorf("second uninstall should be a no-op, got %v", err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
