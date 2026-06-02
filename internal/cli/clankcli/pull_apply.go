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

// applyRemotePull downloads a remote sandbox checkpoint and applies it
// into repoPath: fast-forward check, then committed and uncommitted restore.
func applyRemotePull(ctx context.Context, httpClient *http.Client, repoPath string, mres *syncclient.PullResult) error {
	manifestBytes, err := fetchURL(ctx, httpClient, mres.ManifestURL)
	if err != nil {
		return fmt.Errorf("fetch manifest: %w", err)
	}
	manifest, err := checkpoint.UnmarshalManifest(manifestBytes)
	if err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}

	// Download the head chain (oldest→newest) and load each bundle's
	// objects in order, so manifest.HeadCommit is present for the ancestry
	// check BEFORE the destructive apply. Loading in order satisfies each
	// incremental's base from the bundle before it.
	headReaders := make([]io.Reader, len(mres.HeadBundles))
	for i, hb := range mres.HeadBundles {
		b, err := fetchURL(ctx, httpClient, hb.GetURL)
		if err != nil {
			return fmt.Errorf("fetch head bundle %d: %w", i, err)
		}
		tmp, err := writeTempFile("clank-pull-head-*.bundle", b)
		if err != nil {
			return err
		}
		defer os.Remove(tmp)
		if err := git.FetchBundleObjects(repoPath, tmp); err != nil {
			return fmt.Errorf("load remote objects (bundle %d): %w", i, err)
		}
		headReaders[i] = bytes.NewReader(b)
	}

	localHEAD, err := git.HeadCommit(repoPath)
	if err != nil {
		localHEAD = "" // empty repo; any remote commit is fast-forwardable
	}
	if localHEAD != "" {
		ff, err := git.IsAncestor(repoPath, localHEAD, manifest.HeadCommit)
		if err != nil {
			return fmt.Errorf("fast-forward check: %w", err)
		}
		if !ff {
			return fmt.Errorf(
				"local has diverged from the sandbox (local %s, sandbox %s) — reconcile with git first (e.g. commit/rebase or reset to a fast-forwardable state), then `clank pull`",
				shortSHA(localHEAD), shortSHA(manifest.HeadCommit))
		}
	}

	incrBytes, err := fetchURL(ctx, httpClient, mres.UncommittedURL)
	if err != nil {
		return fmt.Errorf("fetch uncommitted bundle: %w", err)
	}
	if err := checkpoint.Apply(ctx, repoPath, manifest, headReaders, bytes.NewReader(incrBytes)); err != nil {
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
