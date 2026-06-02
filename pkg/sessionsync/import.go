package sessionsync

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/pkg/sync/checkpoint"
)

// ImportWorktreeSessions downloads the session manifest + per-session
// export blobs from the presigned GET URLs and imports each opencode
// session into local storage under destDir (the laptop's worktree
// path), rebasing each blob's directory so opencode files it under the
// destination project. Daemon-free: `opencode import` only, no
// clank-host. Returns the imported opencode external ids.
//
// The symmetric counterpart to ExportWorktreeSessions + UploadSessions.
// Claude sessions in the manifest are skipped (transfer not implemented).
func ImportWorktreeSessions(ctx context.Context, client *http.Client, manifestURL string, sessionBlobURLs map[string]string, destDir string) ([]string, error) {
	if manifestURL == "" {
		return nil, fmt.Errorf("import worktree sessions: manifestURL is required")
	}
	if destDir == "" {
		return nil, fmt.Errorf("import worktree sessions: destDir is required")
	}

	manifestBytes, err := getURL(ctx, client, manifestURL)
	if err != nil {
		return nil, fmt.Errorf("fetch session manifest: %w", err)
	}
	manifest, err := checkpoint.UnmarshalSessionManifest(manifestBytes)
	if err != nil {
		return nil, fmt.Errorf("parse session manifest: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "clank-session-import-*")
	if err != nil {
		return nil, fmt.Errorf("import sessions: tempdir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	oc := OpenCodeBackend{}
	imported := make([]string, 0, len(manifest.Sessions))
	for _, entry := range manifest.Sessions {
		if entry.Backend != agent.BackendOpenCode {
			continue
		}
		url, ok := sessionBlobURLs[entry.SessionID]
		if !ok {
			return nil, fmt.Errorf("missing blob URL for session %s", entry.SessionID)
		}
		blobBytes, err := getURL(ctx, client, url)
		if err != nil {
			return nil, fmt.Errorf("fetch session %s blob: %w", entry.SessionID, err)
		}
		blobPath := filepath.Join(tmpDir, entry.SessionID+".json")
		if err := os.WriteFile(blobPath, blobBytes, 0o600); err != nil {
			return nil, fmt.Errorf("write blob %s: %w", entry.SessionID, err)
		}
		rewritten, err := RewriteImportBlob(blobPath, destDir)
		if err != nil {
			return nil, fmt.Errorf("rewrite session %s: %w", entry.SessionID, err)
		}
		extID, err := oc.ImportSession(ctx, "", rewritten)
		if err != nil {
			return nil, fmt.Errorf("import session %s: %w", entry.SessionID, err)
		}
		imported = append(imported, extID)
	}
	return imported, nil
}

func getURL(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return nil, fmt.Errorf("GET %d: %s", resp.StatusCode, string(preview))
	}
	return io.ReadAll(resp.Body)
}
