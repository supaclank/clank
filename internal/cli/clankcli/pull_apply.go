package clankcli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/acksell/clank/internal/git"
	"github.com/acksell/clank/pkg/sessionsync"
	"github.com/acksell/clank/pkg/sync/checkpoint"
	syncclient "github.com/acksell/clank/pkg/sync/client"
)

// applyRemotePull downloads a materialized sandbox checkpoint and applies
// it into repoPath: it loads the remote history, refuses (without
// touching the worktree) if local has diverged, then restores the
// committed + uncommitted state and imports opencode sessions.
//
// The caller MUST have verified a clean working tree first — the
// fast-forward check + clean tree are what make the hard restore safe.
func applyRemotePull(ctx context.Context, httpClient *http.Client, repoPath string, mres *syncclient.PullResult) error {
	manifestBytes, err := fetchURL(ctx, httpClient, mres.ManifestURL)
	if err != nil {
		return fmt.Errorf("fetch manifest: %w", err)
	}
	manifest, err := checkpoint.UnmarshalManifest(manifestBytes)
	if err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}

	// Download the head-history bundle and load its objects so ancestry
	// is testable BEFORE the destructive apply.
	headBytes, err := fetchURL(ctx, httpClient, mres.HeadCommitURL)
	if err != nil {
		return fmt.Errorf("fetch head bundle: %w", err)
	}
	headTmp, err := writeTempFile("clank-pull-head-*.bundle", headBytes)
	if err != nil {
		return err
	}
	defer os.Remove(headTmp)
	if err := git.FetchBundleObjects(repoPath, headTmp); err != nil {
		return fmt.Errorf("load remote objects: %w", err)
	}

	localHEAD, err := git.HeadCommit(repoPath)
	if err != nil {
		return fmt.Errorf("resolve local HEAD: %w", err)
	}
	ff, err := git.IsAncestor(repoPath, localHEAD, manifest.HeadCommit)
	if err != nil {
		return fmt.Errorf("fast-forward check: %w", err)
	}
	if !ff {
		return fmt.Errorf(
			"local has diverged from the sandbox (local %s, sandbox %s) — reconcile with git first (e.g. commit/rebase or reset to a fast-forwardable state), then `clank pull`",
			shortSHA(localHEAD), shortSHA(manifest.HeadCommit))
	}

	incrBytes, err := fetchURL(ctx, httpClient, mres.IncrementalURL)
	if err != nil {
		return fmt.Errorf("fetch incremental bundle: %w", err)
	}
	if err := checkpoint.Apply(ctx, repoPath, manifest, bytes.NewReader(headBytes), bytes.NewReader(incrBytes)); err != nil {
		return fmt.Errorf("apply checkpoint: %w", err)
	}

	if mres.SessionManifestURL != "" {
		if _, err := sessionsync.ImportWorktreeSessions(ctx, httpClient, mres.SessionManifestURL, mres.SessionBlobURLs, repoPath); err != nil {
			return fmt.Errorf("import sessions: %w", err)
		}
	}
	return nil
}

func fetchURL(ctx context.Context, client *http.Client, url string) ([]byte, error) {
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

func writeTempFile(pattern string, data []byte) (string, error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("write temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}
