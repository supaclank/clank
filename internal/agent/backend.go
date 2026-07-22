package agent

import (
	"fmt"
	"slices"
	"strings"
)

// DefaultBackend is the backend used when neither a CLI flag nor a user
// preference specifies one. Centralised so we don't hard-code "opencode"
// at every entry point.
const DefaultBackend = BackendOpenCode

// AllBackends lists every backend the daemon knows how to launch, in a
// stable display order. Used by the settings UI to cycle / pick.
var AllBackends = []BackendType{
	BackendOpenCode,
	BackendClaudeCode,
}

// ParseBackend resolves a user-facing backend name (CLI flag, settings
// file) to a BackendType. The shorthand "claude" is accepted as an alias
// for "claude-code" for ergonomic CLI usage.
//
// An empty string is rejected — callers should decide whether "" means
// "use the default" (see ResolveBackendPreference) or is a hard error.
func ParseBackend(s string) (BackendType, error) {
	switch s {
	case string(BackendOpenCode):
		return BackendOpenCode, nil
	case string(BackendClaudeCode), "claude":
		return BackendClaudeCode, nil
	case string(BackendCodex):
		return BackendCodex, nil
	case "":
		return "", fmt.Errorf("backend name is empty")
	default:
		return "", fmt.Errorf("unknown backend: %s (valid: opencode, claude, claude-code, codex)", s)
	}
}

// ParseBackendSet parses a comma-separated backend list (the
// --acp-backends flag format). "none" or an empty string yields an empty
// set; "all" expands to AllBackends. Individual names go through
// ParseBackend, so aliases work. Duplicates collapse; unknown names error.
func ParseBackendSet(csv string) ([]BackendType, error) {
	switch strings.TrimSpace(csv) {
	case "", "none":
		return nil, nil
	case "all":
		return slices.Clone(AllBackends), nil
	}
	var set []BackendType
	for _, tok := range strings.Split(csv, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		bt, err := ParseBackend(tok)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(set, bt) {
			set = append(set, bt)
		}
	}
	return set, nil
}

// ResolveBackendPreference turns the raw string from preferences.json
// into a BackendType, falling back to DefaultBackend on empty input. An
// invalid value is treated as "fall back to default" rather than a hard
// error so a corrupt prefs file never bricks the TUI/CLI — callers get
// back a non-nil err they can surface as a warning.
func ResolveBackendPreference(s string) (BackendType, error) {
	if s == "" {
		return DefaultBackend, nil
	}
	bt, err := ParseBackend(s)
	if err != nil {
		return DefaultBackend, err
	}
	return bt, nil
}
