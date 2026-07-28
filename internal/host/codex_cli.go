package host

// The codex CLI's own login state as a status signal. Unlike the
// Anthropic sinks, a ChatGPT-subscription credential has no env-var
// channel: codex only reads it from $CODEX_HOME/auth.json, which codex
// itself writes on login and refreshes in place. clank therefore
// treats that file as the credential's home and probes it by presence
// only — no token bytes are ever read, decoded, or copied.

import (
	"os"
	"path/filepath"
)

// EnvCodexHome is the codex CLI's home-directory override. The login
// ceremony pins it on the subprocess so a host-level override, the
// spawned adapter (which inherits the host environment), and status
// probes all agree on one directory.
const EnvCodexHome = "CODEX_HOME"

// codexAuthFile is the credential file codex maintains inside its home.
const codexAuthFile = "auth.json"

// codexHome resolves the directory the codex CLI keeps its state in:
// $CODEX_HOME when the host process carries it, else ~/.codex.
func (a *AuthManager) codexHome() string {
	if dir := a.lookupEnv(EnvCodexHome); dir != "" {
		return dir
	}
	return filepath.Join(a.homeDir, ".codex")
}

// codexAuthJSONPath is where codex stores its login inside codexHome.
func (a *AuthManager) codexAuthJSONPath() string {
	return filepath.Join(a.codexHome(), codexAuthFile)
}

// codexCLILoginPresent reports whether a codex login exists that the
// codex-acp adapter's app-server child would pick up. Stat-only:
// presence and non-emptiness, never the contents.
func codexCLILoginPresent(authJSONPath string) bool {
	fi, err := os.Stat(authJSONPath)
	return err == nil && fi.Mode().IsRegular() && fi.Size() > 0
}
