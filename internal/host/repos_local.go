package host

// Discovered local checkouts — the laptop half of the repo listing.
// On a laptop host the user's own folders (~/github.com/acme/api, …)
// are the repos that matter; clank is a guest on that filesystem, not
// its owner. Their identity is the checkout's absolute root path,
// base64url-encoded into a slug — no registry, no minted id, no state:
// decoding the slug and finding a git repo there IS the existence
// check, the same read-through-derivation contract the canonical
// layout uses. The recents set is mined from session history (every
// session row carries project_dir), so a repo the user works in shows
// up on the phone with zero import action.
//
// clank never mutates a discovered checkout beyond `git worktree add`
// bookkeeping: sessions run in linked worktrees under ~/work/<ULID>,
// which share refs and objects with the user's repo live — branches
// they create locally are visible to clank worktrees instantly, and
// vice versa, by construction.

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/acksell/clank/internal/git"
	"github.com/acksell/clank/internal/repolabel"
)

// maxRepoSlugLength bounds any repo slug at the HTTP boundary. Sized
// for a local-checkout slug: base64url of a Linux PATH_MAX (4096-byte,
// so 4095 usable) path is 5460 chars — below that, a legitimate deep
// checkout would mint a slug ListRepos returns but resolveRepoSlug then
// rejects as invalid.
const maxRepoSlugLength = 5460

// maxDiscoveredLocalRepos bounds how many distinct checkouts a listing
// mines from session history. Sessions are read most-recently-updated
// first, so the cap keeps the listing to recents rather than growing
// unbounded (and unresponsive) over months of history.
const maxDiscoveredLocalRepos = 30

// localInfoConcurrency bounds the per-checkout git subprocess fan-out in
// discoveredLocalRepos, mirroring checkRollupConcurrency's cap on the
// overview's PR-check fan-out.
const localInfoConcurrency = 8

// MaxDiscoveredLocalReposForTest exposes maxDiscoveredLocalRepos to the
// external host_test package. Test-only.
const MaxDiscoveredLocalReposForTest = maxDiscoveredLocalRepos

// LocalRepoSlug encodes an absolute folder path as its routing key —
// the repo slug for local checkouts, and the preview key `clank
// preview` mints for the folder it serves (which may be a monorepo
// subdir, so no root normalization here).
// base64url stays inside the slug alphabet, is lossless, and
// needs no lookup table to reverse — the path IS the identity, so a
// moved folder is honestly a new repo (matching how IDE recents work).
func LocalRepoSlug(root string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(root))
}

// localRepoPath decodes a local-checkout slug back to the absolute,
// clean path it names. ok=false means the slug cannot name a checkout
// (not base64url, or not an absolute clean path).
func localRepoPath(slug string) (string, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(slug)
	if err != nil {
		return "", false
	}
	p := string(raw)
	if !filepath.IsAbs(p) || p != filepath.Clean(p) {
		return "", false
	}
	return p, true
}

// localCheckoutRoot resolves dir to the repo identity every local-
// checkout flow keys on: the MAIN worktree's root, symlink-resolved.
// show-toplevel would return a linked worktree's own path, so a repo
// the user has ten worktrees of would list as ten repos — the main
// root collapses them all into one — and symlink resolution keeps two
// spellings of one folder (/tmp vs /private/tmp on macOS) from listing
// twice. Bare repos (old sync mirrors, repo-first canonicals) are
// rejected: a local checkout is by definition a work tree.
func localCheckoutRoot(dir string) (string, error) {
	root, err := git.MainWorktreeRoot(dir)
	if err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve symlinks: %w", err)
	}
	if fi, err := os.Stat(filepath.Join(root, ".git")); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%q is not a non-bare repo root", root)
	}
	return root, nil
}

// discoveredLocalRepos derives the local-checkout repo list from
// session history: every session's project_dir, filtered to dirs that
// still exist, sit outside ~/work (worktrees of canonicals are already
// listed via their repo), and resolve to a git root. Best-effort by
// design — a host without a sessions store simply has no history to
// mine.
func (s *Service) discoveredLocalRepos(ctx context.Context) ([]RepoInfo, error) {
	if s.sessionsStore == nil {
		return nil, nil
	}
	sessions, err := s.sessionsStore.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	workRoot, err := s.workRootDir()
	if err != nil {
		return nil, err
	}
	// localCheckoutRoot symlink-resolves every candidate root before the
	// containment checks below run — workRoot must match that resolution,
	// or a clank-owned worktree reached through a symlinked home (macOS
	// /tmp vs /private/tmp) slips past the filter and leaks as a "local
	// repo". A workRoot that doesn't exist yet (fresh host, no worktrees
	// created) can't contain anything either way, so that failure mode is
	// not an error.
	if resolved, err := filepath.EvalSymlinks(workRoot); err == nil {
		workRoot = resolved
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("resolve work root symlinks: %w", err)
	}

	dirSeen := map[string]bool{}
	rootSeen := map[string]bool{}
	var roots []string
	for _, sess := range sessions {
		if len(roots) >= maxDiscoveredLocalRepos {
			break // sessions are most-recently-updated first — keep the recents
		}
		dir := sess.GitRef.LocalPath
		if dir == "" || !filepath.IsAbs(dir) || dirSeen[dir] {
			continue
		}
		dirSeen[dir] = true
		// ~/work dirs are canonicals' worktrees — their repo is already
		// in the canonical half of the listing.
		if dir == workRoot || strings.HasPrefix(dir, workRoot+string(filepath.Separator)) {
			continue
		}
		if _, statErr := os.Stat(dir); statErr != nil {
			continue // folder is gone — stale session row, not a repo
		}
		// TODO(ai-review): localCheckoutRoot spawns a git subprocess per
		// candidate dir, sequentially — bounded to maxDiscoveredLocalRepos
		// valid roots, but a host with many stale/scratch session dirs
		// interleaved with real repos still pays one sequential spawn per
		// dir scanned (not just per root kept) before hitting the cap.
		// https://github.com/Acksell/clank/pull/161#discussion_r3599240729
		root, rootErr := localCheckoutRoot(dir)
		if rootErr != nil {
			continue // sessions in non-repo scratch dirs (or bare repos) aren't checkouts
		}
		if root == workRoot || strings.HasPrefix(root, workRoot+string(filepath.Separator)) {
			continue // resolves into clank's own layout — not the user's
		}
		if rootSeen[root] {
			continue
		}
		rootSeen[root] = true
		roots = append(roots, root)
	}
	// roots is already in the order clients want: sessions arrive
	// most-recently-updated first, so first-seen == most recently used
	// (IDE-recents ordering). Do not re-sort.

	// Each root spawns several git subprocesses (localRepoInfoFor) — run
	// them concurrently, capped at localInfoConcurrency, so listing
	// latency is one round-trip and not len(roots) of them, without
	// bursting every git subprocess at once.
	results := make([]*RepoInfo, len(roots))
	var wg sync.WaitGroup
	sem := make(chan struct{}, localInfoConcurrency)
	for i, root := range roots {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, root string) {
			defer wg.Done()
			defer func() { <-sem }()
			info, infoErr := s.localRepoInfoFor(root)
			if infoErr != nil {
				// One broken checkout must not take the whole listing down.
				s.log.Printf("list repos: skip local checkout %q: %v", root, infoErr)
				return
			}
			results[i] = &info
		}(i, root)
	}
	wg.Wait()

	infos := make([]RepoInfo, 0, len(roots))
	for _, info := range results {
		if info != nil {
			infos = append(infos, *info)
		}
	}
	return infos, nil
}

// localRepoInfoFor assembles one discovered checkout's listing entry.
// Unlike a canonical (whose HEAD is the default branch by clone
// construction), a checkout's HEAD is whatever the user has checked
// out right now — so the default branch is probed instead.
func (s *Service) localRepoInfoFor(root string) (RepoInfo, error) {
	defaultBranch, err := git.DefaultBranch(root)
	if err != nil {
		return RepoInfo{}, err
	}
	worktrees, err := s.stampedWorktreeInfos(root)
	if err != nil {
		return RepoInfo{}, err
	}
	return RepoInfo{
		Slug:            LocalRepoSlug(root),
		Label:           repolabel.ComputeRepoLabel(root),
		Origin:          repoOriginFor(root),
		DefaultBranch:   defaultBranch,
		IsLocalCheckout: true,
		Path:            root,
		Worktrees:       worktrees,
	}, nil
}
