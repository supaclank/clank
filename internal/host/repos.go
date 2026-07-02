package host

// Repo-first storage plumbing. One bare blobless canonical clone per
// repo lives at ~/work/repos/<slug>/repo.git; every ~/work/<worktreeID>
// is a linked `git worktree` of a canonical. This file owns the shared
// pieces: layout paths, slug minting, display labels, and the per-repo
// mutex that serializes canonical mutations.
//
// The SLUG is a stable, single-URL-segment, host-minted routing key —
// minted once at repo creation and NEVER renamed (it doubles as the
// on-disk dir name; renames under running sessions would be chaos). It
// is NOT necessarily owner/repo: a greenfield repo is named before it
// has an origin, and a GitHub rename/transfer changes the label, not
// the slug. The label shown to users derives from the origin remote at
// read time, falling back to the clank.repo-label config stamped at
// creation.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/acksell/clank/internal/git"
	githubpkg "github.com/acksell/clank/internal/host/github"
	"github.com/acksell/clank/internal/repolabel"
)

const (
	// reposDirName is the subdirectory of the work root holding the
	// canonical clones. It can never collide with a worktree dir: those
	// are 26-char ULIDs, and ULIDs are longer than "repos".
	reposDirName = "repos"

	// canonicalRepoDirName is the bare repo's dir name inside its slug
	// dir (~/work/repos/<slug>/repo.git).
	canonicalRepoDirName = "repo.git"

	// repoConfigLabelKey is the git config key carrying a repo's display
	// label when no origin remote exists to derive one from (greenfield
	// pre-publish). Stamped at creation; the origin wins once present.
	repoConfigLabelKey = "clank.repo-label"

	// importSlugSeparator joins owner and repo in an import's slug.
	// sanitizeRepoName collapses runs of disallowed characters to a
	// single '-', so a sanitized name can never contain "__" — making
	// the mapping unambiguous.
	importSlugSeparator = "__"
)

// reposRootDir returns the directory holding the canonical clones
// (~/work/repos), honoring the test work-root override.
func reposRootDir() (string, error) {
	root, err := workRootDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, reposDirName), nil
}

// canonicalGitDir returns the bare canonical's path for slug
// (~/work/repos/<slug>/repo.git).
func canonicalGitDir(slug string) (string, error) {
	root, err := reposRootDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, slug, canonicalRepoDirName), nil
}

// slugForImport mints the slug for an imported owner/repo:
// sanitize(owner)__sanitize(repo). Deterministic, so re-importing the
// same repo lands on the same canonical (idempotency is checked against
// the existing canonical's origin URL by the caller).
func slugForImport(owner, repo string) (string, error) {
	o := sanitizeRepoName(owner)
	r := sanitizeRepoName(repo)
	if o == "" || r == "" {
		return "", fmt.Errorf("%w: owner/repo %q/%q sanitize to nothing usable", ErrInvalidArgument, owner, repo)
	}
	return o + importSlugSeparator + r, nil
}

// slugForName mints a greenfield repo's slug from its display name,
// suffixing -2, -3, … on collision with an existing slug dir. Unlike
// imports there's nothing to be idempotent against — two apps named
// "todo" are two repos.
func slugForName(name string) (string, error) {
	base := sanitizeRepoName(name)
	if base == "" {
		return "", ErrInvalidRepoName
	}
	root, err := reposRootDir()
	if err != nil {
		return "", err
	}
	candidate := base
	for i := 2; ; i++ {
		if _, statErr := os.Stat(filepath.Join(root, candidate)); os.IsNotExist(statErr) {
			return candidate, nil
		} else if statErr != nil {
			return "", fmt.Errorf("check slug dir %q: %w", candidate, statErr)
		}
		candidate = fmt.Sprintf("%s-%d", base, i)
	}
}

// repoLabelFor derives the display label for the canonical at gitDir:
// the origin remote's owner/repo when one exists, else the stamped
// clank.repo-label, else the slug (dir name) as a last resort.
func repoLabelFor(gitDir string) string {
	fallback := filepath.Base(filepath.Dir(gitDir)) // <slug> from <slug>/repo.git
	if remoteURL, err := git.RemoteURL(gitDir, "origin"); err == nil && remoteURL != "" {
		return repolabel.RepoLabelFromURL(remoteURL, fallback)
	}
	if label, err := git.GetLocalConfig(gitDir, repoConfigLabelKey); err == nil && label != "" {
		return label
	}
	return fallback
}

// credentialHelperValue renders the persistent credential.helper config
// value pointing at this binary's git-credential subcommand, or "" (with
// a log line) when the executable can't be resolved — canonicals then
// simply lack always-on auth, the same behavior as a disconnected store.
func (s *Service) credentialHelperValue() string {
	exe, err := os.Executable()
	if err != nil {
		s.log.Printf("warning: resolve executable for credential helper: %v", err)
		return ""
	}
	return githubpkg.GitCredentialHelperValue(exe)
}

// lockRepo serializes canonical mutations (clone, fetch, worktree
// add/remove, branch create, publish's remote-add) per slug. Mirrors
// LockWorktreeSync's lazily-allocated map. Returns the unlock func.
func (s *Service) lockRepo(slug string) func() {
	s.repoLocksMu.Lock()
	mu := s.repoLocks[slug]
	if mu == nil {
		mu = &sync.Mutex{}
		s.repoLocks[slug] = mu
	}
	s.repoLocksMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// worktreeCanonicalGitDir resolves the canonical repo dir a linked
// worktree belongs to, via its git common dir. Errors when wtDir isn't
// a repo; returns ("", false, nil) when it IS a repo but a standalone
// one (legacy independent clone — its common dir is its own .git).
func worktreeCanonicalGitDir(wtDir string) (gitDir string, linked bool, err error) {
	common, err := git.CommonDir(wtDir)
	if err != nil {
		return "", false, err
	}
	if filepath.Base(common) == ".git" {
		return "", false, nil // standalone clone, not a linked worktree
	}
	return common, true, nil
}

// isULIDLike reports whether name is shaped like a worktree ID (26-char
// Crockford ULID). Directory scans over ~/work use it to skip the
// repos/ subtree (and any other non-worktree entry).
func isULIDLike(name string) bool {
	if len(name) != 26 {
		return false
	}
	for _, r := range name {
		if !strings.ContainsRune("0123456789ABCDEFGHJKMNPQRSTVWXYZ", r) {
			return false
		}
	}
	return true
}
