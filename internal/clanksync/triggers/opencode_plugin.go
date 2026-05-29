package triggers

import (
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

//go:embed plugin_template.ts
var pluginTemplate string

// pluginFileName is the autopush plugin's filename under opencode's
// plugins dir.
const pluginFileName = "clank-autopush.ts"

// InstallOpenCodePlugin writes the session.idle plugin into
// opencodeConfigDir/plugins, pointed at clankBin. Idempotent (overwrite).
func InstallOpenCodePlugin(clankBin, opencodeConfigDir string) error {
	dir := filepath.Join(opencodeConfigDir, "plugins")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	// strconv.Quote yields a valid TS string literal even if the path
	// contains spaces or other characters.
	content := strings.ReplaceAll(pluginTemplate, "__CLANK_BIN__", strconv.Quote(clankBin))
	return os.WriteFile(filepath.Join(dir, pluginFileName), []byte(content), 0o644)
}

// UninstallOpenCodePlugin removes the autopush plugin file. No-op if absent.
func UninstallOpenCodePlugin(opencodeConfigDir string) error {
	path := filepath.Join(opencodeConfigDir, "plugins", pluginFileName)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}
