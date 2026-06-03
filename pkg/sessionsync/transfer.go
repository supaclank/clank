package sessionsync

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/acksell/clank/internal/agent"
)

const opencodeBlobExt = ".json"

// blobExt is the file extension for a backend's export blob. Blobs are
// opaque to the transport; the extension is cosmetic but kept faithful to
// each backend's native format.
func blobExt(b agent.BackendType) string {
	switch b {
	case agent.BackendClaudeCode:
		return claudeTranscriptExt
	case agent.BackendOpenCode:
		return opencodeBlobExt
	default:
		panic("sessionsync: no blob extension for backend " + string(b))
	}
}

// ExportSessionBlob exports one session to dst, routed to its backend. cwd
// is the session's working directory; only directory-filed backends (Claude)
// use it — opencode's export is HOME-relative and ignores it (a stray path
// would break its chdir). Shared by the daemon-free export orchestrator and
// internal/host so backend routing lives in one place.
func ExportSessionBlob(ctx context.Context, backend agent.BackendType, cwd, externalID string, dst io.Writer) error {
	be, err := BackendFor(backend)
	if err != nil {
		return err
	}
	exportDir := ""
	if backend == agent.BackendClaudeCode {
		exportDir = cwd
	}
	return be.ExportSession(ctx, exportDir, externalID, dst)
}

// ImportSessionBlob rebases an export blob for destDir (the importing host's
// worktree path) and installs it via its backend, returning the
// backend-native session id. Shared by the daemon-free import orchestrator
// and internal/host. opencode imports HOME-relative (the rebase happens
// inside the blob's info.directory); Claude writes the transcript under
// destDir's encoded path. The rewrite is the only mutation — see
// RewriteImportBlob / RewriteClaudeImportBlob.
func ImportSessionBlob(ctx context.Context, backend agent.BackendType, blobPath, destDir string) (string, error) {
	switch backend {
	case agent.BackendOpenCode:
		rewritten, err := RewriteImportBlob(blobPath, destDir)
		if err != nil {
			return "", err
		}
		defer os.Remove(rewritten)
		return OpenCodeBackend{}.ImportSession(ctx, "", rewritten)
	case agent.BackendClaudeCode:
		rewritten, err := RewriteClaudeImportBlob(blobPath, destDir)
		if err != nil {
			return "", err
		}
		defer os.Remove(rewritten)
		return ClaudeBackend{}.ImportSession(ctx, destDir, rewritten)
	default:
		return "", fmt.Errorf("sessionsync: no import for backend %q", backend)
	}
}
