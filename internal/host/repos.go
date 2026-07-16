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
// lockWorktree's lazily-allocated map. Returns the unlock func.
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

// validSlug reports whether s is safe to use as a repo routing key —
// a value slugForImport/slugForName could have minted, or a local-
// checkout slug (base64url path, see repos_local.go; hence the length
// bound). The check matters at the HTTP boundary: the slug becomes a
// path segment under ~/work/repos, so anything outside the sanitize
// alphabet (or the "." / ".." specials, which the alphabet would
// otherwise admit) must be rejected before it reaches filepath.Join.
func validSlug(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > maxRepoSlugLength {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// resolvedRepo is a slug resolved to the git dir repo-scoped operations
// run against: a bare canonical (~/work/repos/<slug>/repo.git) or a
// discovered local checkout's root. Slug is normalized — for local
// checkouts it re-encodes the resolved root, so two spellings of the
// same repo share one lock key.
type resolvedRepo struct {
	gitDir        string
	slug          string
	localCheckout bool
}

// resolveRepoSlug validates slug and resolves it: the canonical wins
// when ~/work/repos/<slug> exists, else the slug is decoded as a
// local-checkout path (repos_local.go) and verified to still be a git
// repo. ErrRepoNotFound when neither resolves; ErrInvalidArgument for
// a malformed slug.
func resolveRepoSlug(slug string) (resolvedRepo, error) {
	if !validSlug(slug) {
		return resolvedRepo{}, fmt.Errorf("%w: invalid repo slug %q", ErrInvalidArgument, slug)
	}
	gitDir, err := canonicalGitDir(slug)
	if err != nil {
		return resolvedRepo{}, err
	}
	switch _, statErr := os.Stat(gitDir); {
	case statErr == nil:
		return resolvedRepo{gitDir: gitDir, slug: slug}, nil
	case !os.IsNotExist(statErr):
		return resolvedRepo{}, fmt.Errorf("stat canonical %q: %w", slug, statErr)
	}
	path, ok := localRepoPath(slug)
	if !ok {
		return resolvedRepo{}, fmt.Errorf("%w: %q", ErrRepoNotFound, slug)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		// Moved or deleted folder — an honest 404, like a removed canonical.
		return resolvedRepo{}, fmt.Errorf("%w: %q", ErrRepoNotFound, slug)
	}
	root, err := git.RepoRoot(path)
	if err != nil {
		return resolvedRepo{}, fmt.Errorf("%w: %q", ErrRepoNotFound, slug)
	}
	return resolvedRepo{gitDir: root, slug: localRepoSlug(root), localCheckout: true}, nil
}

// ensureRepoBranchAvailable makes branch resolvable in the canonical at
// gitDir: a no-op when refs/heads/<branch> or refs/remotes/origin/<branch>
// already exists, else one single-branch fetch from origin into the
// remote-tracking namespace. Auth is resolved from the origin itself:
// github.com origins use the stored token (ErrGitHubNotConnected when
// absent — a fetch was genuinely needed and can't happen); other
// origins (local bare repos in tests) fetch verbatim; NO origin at all
// (greenfield) is a no-op — there's nowhere to fetch from, and the
// caller's worktree add reports ErrNotFound with full context. A branch
// missing on the remote likewise falls through to the caller.
//
// Caller holds the repo lock.
func (s *Service) ensureRepoBranchAvailable(gitDir, branch string) error {
	local, err := git.BranchExists(gitDir, branch)
	if err != nil {
		return fmt.Errorf("check branch: %w", err)
	}
	if local {
		return nil
	}
	tracking, err := git.RemoteTrackingBranchExists(gitDir, "origin", branch)
	if err != nil {
		return fmt.Errorf("check remote branch: %w", err)
	}
	if tracking {
		return nil
	}

	remoteURL, err := git.RemoteURL(gitDir, "origin")
	if err != nil {
		if !git.IsRemoteNotConfigured(err) {
			return fmt.Errorf("get origin url: %w", err)
		}
		return nil // greenfield: no origin to fetch from
	}
	fetchURL := remoteURL
	authHeader := ""
	if owner, repo, perr := githubpkg.ParseGitHubRemote(remoteURL); perr == nil {
		if s.github == nil {
			return ErrGitHubManagerUnavailable
		}
		creds, cerr := s.github.Store().Read()
		if cerr != nil {
			return fmt.Errorf("read github credentials: %w", cerr)
		}
		if creds.AccessToken == "" {
			return ErrGitHubNotConnected
		}
		fetchURL = fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)
		authHeader = buildAuthHeader(creds.AccessToken)
	}
	refspec := "+refs/heads/" + branch + ":refs/remotes/origin/" + branch
	if err := git.Fetch(gitDir, fetchURL, refspec, git.PushOptions{ExtraHeader: authHeader}); err != nil {
		if isNoRemoteRef(err) {
			return nil // absent on the remote too → caller reports ErrNotFound
		}
		return fmt.Errorf("fetch branch %q: %w", branch, err)
	}
	return nil
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
