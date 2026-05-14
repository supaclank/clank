package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/acksell/clank/pkg/provisioner"
	clanksync "github.com/acksell/clank/pkg/sync"
	"github.com/acksell/clank/pkg/sync/checkpoint"
)

// pushSessionsToSprite is the session leg of pushCheckpointToSprite.
// Called after the code apply succeeds, before TransferOwnership.
//
// Flow:
//  1. Gateway mints a GET URL for session-manifest.json and downloads it.
//     - If the manifest is missing (404), the checkpoint has no sessions
//       (pre-feature or empty worktree) and we return nil silently.
//  2. Gateway mints per-session GET URLs for every SessionEntry in the
//     manifest.
//  3. Gateway POSTs to the sprite's /sync/sessions/apply-from-urls. The
//     sprite downloads each session blob and calls
//     Service.RegisterImportedSession.
//
// Bundle bytes — including session blobs — never traverse this process
// in the steady-state. The manifest is the one exception: the gateway
// reads it inline so it can mint per-session URLs in step 2.
func (g *Gateway) pushSessionsToSprite(ctx context.Context, cli *http.Client, hostRef provisioner.HostRef, wt clanksync.Worktree, userID string) error {
	// Step 1: mint just the manifest URL.
	dl, err := g.cfg.Sync.DownloadSessionURLs(ctx, userID, wt.LatestSyncedCheckpoint, nil)
	if err != nil {
		return fmt.Errorf("download session manifest URL: %w", err)
	}

	// Step 1b: fetch the manifest.
	manifestBytes, status, err := fetchURL(ctx, cli, dl.SessionManifestGetURL)
	if err != nil {
		if status == http.StatusNotFound {
			g.log.Printf("gateway migrate: no session manifest for checkpoint %s, skipping session leg", wt.LatestSyncedCheckpoint)
			return nil
		}
		return fmt.Errorf("fetch session manifest: %w", err)
	}
	manifest, err := checkpoint.UnmarshalSessionManifest(manifestBytes)
	if err != nil {
		return fmt.Errorf("parse session manifest: %w", err)
	}
	if len(manifest.Sessions) == 0 {
		return nil // empty manifest — nothing to do
	}

	// Step 2: mint per-session GET URLs.
	sessionIDs := make([]string, len(manifest.Sessions))
	for i, e := range manifest.Sessions {
		sessionIDs[i] = e.SessionID
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		dl2, err := g.cfg.Sync.DownloadSessionURLs(ctx, userID, wt.LatestSyncedCheckpoint, sessionIDs)
		if err != nil {
			return fmt.Errorf("download session blob URLs: %w", err)
		}

		// Step 3: POST to sprite.
		code, err := applySessionsFromURLsToSprite(ctx, cli, hostRef.URL, hostRef.Transport, hostRef.AuthToken, wt.ID, dl2.SessionManifestGetURL, dl2.SessionGetURLs)
		if err == nil {
			return nil
		}
		lastErr = err
		if code == "url_expired" && attempt == 0 {
			g.log.Printf("gateway migrate: sprite reported session url_expired, retrying with fresh URLs")
			continue
		}
		return err
	}
	return fmt.Errorf("apply sessions to sprite: exhausted retries: %w", lastErr)
}

// applySessionsFromURLsToSprite POSTs the session-apply request to
// the sprite's /sync/sessions/apply-from-urls. Returns a typed error
// code from the sprite (see internal/host/mux/sessions_sync.go) so
// the caller can branch on "url_expired" for a one-shot retry.
func applySessionsFromURLsToSprite(ctx context.Context, baseClient *http.Client, spriteURL string, transport http.RoundTripper, authToken, worktreeID, manifestURL string, sessionURLs map[string]string) (errCode string, err error) {
	if worktreeID == "" {
		return "", errors.New("worktree id is required")
	}
	if manifestURL == "" {
		return "", errors.New("session manifest URL is required")
	}
	body, err := json.Marshal(map[string]any{
		"worktree_id":          worktreeID,
		"session_manifest_url": manifestURL,
		"session_blob_urls":    sessionURLs,
	})
	if err != nil {
		return "", fmt.Errorf("marshal body: %w", err)
	}
	target := strings.TrimRight(spriteURL, "/") + "/sync/sessions/apply-from-urls"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	cli := baseClient
	if transport != nil {
		cli = &http.Client{Transport: transport, Timeout: baseClient.Timeout}
	}
	resp, err := cli.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return "", nil
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	var parsed struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(respBody, &parsed)
	if parsed.Code != "" {
		return parsed.Code, fmt.Errorf("sprite sessions/apply-from-urls (%d, code=%s): %s", resp.StatusCode, parsed.Code, parsed.Error)
	}
	return "", fmt.Errorf("sprite sessions/apply-from-urls %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
}

// fetchURL GETs blobURL and returns body + the HTTP status. Used by
// pushSessionsToSprite to read the session manifest inline; the actual
// session blobs go straight from S3 to the sprite without ever touching
// gateway memory.
func fetchURL(ctx context.Context, cli *http.Client, blobURL string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, blobURL, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Surface the response body — S3 SigV4 / object-not-found
		// errors are XML and carry the actual reason. Without it
		// the operator gets an opaque "GET 403".
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, resp.StatusCode, fmt.Errorf("GET %d: %s", resp.StatusCode, strings.TrimSpace(string(preview)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // cap at 1 MiB
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

