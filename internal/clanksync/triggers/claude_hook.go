package triggers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// claudeHookMarker tags clank's Stop-hook command so re-install and
// uninstall can find it without disturbing the user's other hooks.
const claudeHookMarker = "clank-autopush"

// InstallClaudeHook idempotently merges a Stop hook into claudeDir's
// settings.json that fires `clank push` on each turn end. The user's
// existing hooks are preserved; an earlier clank entry is replaced.
//
// The command self-backgrounds (nohup … &) so Claude never waits on the
// push, and logs to a temp file for debugging. The agent is never
// blocked or broken by a slow/failed push.
func InstallClaudeHook(clankBin, claudeDir string) error {
	path := filepath.Join(claudeDir, "settings.json")
	settings, err := readJSONObject(path)
	if err != nil {
		return err
	}

	hooks := childObject(settings, "hooks")
	stop, _ := hooks["Stop"].([]any)

	entry := map[string]any{
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": claudeHookCommand(clankBin),
				"timeout": 30,
			},
		},
	}

	replaced := false
	for i, g := range stop {
		if groupHasMarker(g) {
			stop[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		stop = append(stop, entry)
	}
	hooks["Stop"] = stop
	settings["hooks"] = hooks

	return writeJSONObject(claudeDir, path, settings)
}

// UninstallClaudeHook removes clank's Stop hook, leaving other hooks
// intact. No-op if settings.json or the hook is absent.
func UninstallClaudeHook(claudeDir string) error {
	path := filepath.Join(claudeDir, "settings.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		return nil
	}
	stop, _ := hooks["Stop"].([]any)
	kept := stop[:0]
	for _, g := range stop {
		if !groupHasMarker(g) {
			kept = append(kept, g)
		}
	}
	if len(kept) == 0 {
		delete(hooks, "Stop")
	} else {
		hooks["Stop"] = kept
	}
	settings["hooks"] = hooks
	return writeJSONObject(claudeDir, path, settings)
}

// claudeHookCommand is the shell command run on Stop. It backgrounds
// clank so the agent isn't blocked, redirects output to a log, and
// carries the marker as a trailing comment for idempotency.
func claudeHookCommand(clankBin string) string {
	return fmt.Sprintf(`nohup %q push "$CLAUDE_PROJECT_DIR" >>"${TMPDIR:-/tmp}/clank-autopush.log" 2>&1 & # %s`, clankBin, claudeHookMarker)
}

func groupHasMarker(g any) bool {
	gm, ok := g.(map[string]any)
	if !ok {
		return false
	}
	hooks, _ := gm["hooks"].([]any)
	for _, h := range hooks {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if cmd, _ := hm["command"].(string); strings.Contains(cmd, claudeHookMarker) {
			return true
		}
	}
	return false
}

// readJSONObject reads a JSON object file, returning an empty object if
// the file is absent.
func readJSONObject(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	obj := map[string]any{}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &obj); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	return obj, nil
}

func writeJSONObject(dir, path string, obj map[string]any) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	out, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

// childObject returns obj[key] as a map, creating it if missing or not
// an object.
func childObject(obj map[string]any, key string) map[string]any {
	if c, ok := obj[key].(map[string]any); ok {
		return c
	}
	c := map[string]any{}
	obj[key] = c
	return c
}
