package sessionsync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"

	"github.com/acksell/clank/internal/agent"
)

// opencodeListEntry mirrors one element of `opencode session list
// --format json`. `updated`/`created` are epoch milliseconds.
type opencodeListEntry struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Updated   int64  `json:"updated"`
	Created   int64  `json:"created"`
	ProjectID string `json:"projectId"`
	Directory string `json:"directory"`
}

// runOpenCodeSessionList runs `opencode session list --format json` and
// decodes the result. The list is GLOBAL (every project), so callers
// scope by Directory. Hermetic — reads opencode's local storage, no
// server/credentials/network.
func runOpenCodeSessionList(ctx context.Context) ([]DiscoveredSession, error) {
	cmd := exec.CommandContext(ctx, "opencode", "session", "list", "--format", "json")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("opencode session list: %w: %s", err, stderr.String())
	}
	var entries []opencodeListEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		return nil, fmt.Errorf("opencode session list: decode json: %w", err)
	}
	out := make([]DiscoveredSession, 0, len(entries))
	for _, e := range entries {
		out = append(out, DiscoveredSession{
			Backend:    agent.BackendOpenCode,
			ExternalID: e.ID,
			Title:      e.Title,
			ProjectDir: e.Directory,
			CreatedAt:  millisToTime(e.Created),
			UpdatedAt:  millisToTime(e.Updated),
		})
	}
	return out, nil
}

// samePath reports whether a and b denote the same directory after
// resolving symlinks (macOS reports /private/tmp where the worktree
// path is /tmp) and cleaning. Used to scope the global opencode session
// list to one worktree.
func samePath(a, b string) bool {
	return normalizePath(a) == normalizePath(b)
}

func normalizePath(p string) string {
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return filepath.Clean(p)
}
