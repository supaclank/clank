package sessionsync

import (
	"context"
	"io"

	"github.com/acksell/clank/internal/agent"
)

// OpenCodeBackend is the daemon-free opencode session source. Discovery
// uses `opencode session list`; export/import delegate to the hermetic
// subprocess wrappers in internal/agent.
type OpenCodeBackend struct{}

func (OpenCodeBackend) Type() agent.BackendType { return agent.BackendOpenCode }

// ListSessions returns opencode sessions whose project directory matches
// projectDir. The underlying `opencode session list` is global, so we
// filter here; an empty projectDir returns every session.
func (OpenCodeBackend) ListSessions(ctx context.Context, projectDir string) ([]DiscoveredSession, error) {
	all, err := runOpenCodeSessionList(ctx)
	if err != nil {
		return nil, err
	}
	if projectDir == "" {
		return all, nil
	}
	scoped := make([]DiscoveredSession, 0, len(all))
	for _, s := range all {
		if samePath(s.ProjectDir, projectDir) {
			scoped = append(scoped, s)
		}
	}
	return scoped, nil
}

func (OpenCodeBackend) ExportSession(ctx context.Context, projectDir, externalID string, dst io.Writer) error {
	return agent.OpenCodeExportSession(ctx, projectDir, externalID, dst)
}

func (OpenCodeBackend) ImportSession(ctx context.Context, projectDir, blobPath string) (string, error) {
	return agent.OpenCodeImportSession(ctx, projectDir, blobPath)
}
