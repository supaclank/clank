package host

import "errors"

// Sentinel errors for Service methods. Callers (e.g. HTTP handlers in
// host/mux) use errors.Is to translate these into appropriate status
// codes without coupling to string matching.
var (
	// ErrNotFound is returned when a requested repo, branch, or worktree
	// does not exist on the host.
	ErrNotFound = errors.New("host: not found")

	// ErrWorktreeBusy is returned when a destructive worktree operation
	// (e.g. DeleteWorktree, DeleteRepo) finds a session actively running
	// on it. The caller should retry once the session goes idle.
	ErrWorktreeBusy = errors.New("host: worktree has an active session")

	// ErrCannotMergeDefault is returned when MergeBranch is called with
	// the default branch as its target (you cannot merge a branch into
	// itself).
	ErrCannotMergeDefault = errors.New("host: cannot merge the default branch into itself")

	// ErrNothingToMerge is returned when MergeBranch finds the feature
	// branch has no commits ahead and a clean worktree.
	ErrNothingToMerge = errors.New("host: nothing to merge")

	// ErrCommitMessageRequired is returned when MergeBranch finds
	// uncommitted work in the feature worktree but no commit message was
	// supplied for the auto-commit.
	ErrCommitMessageRequired = errors.New("host: commit_message is required when worktree has uncommitted changes")

	// ErrTargetDirty is returned when MergeBranch finds the merge
	// target's worktree has uncommitted changes. Named branch-agnostic
	// because the target may be any branch (default branch, a release
	// branch, etc.) — not always "main".
	ErrTargetDirty = errors.New("host: merge target worktree has uncommitted changes; commit or stash them first")

	// ErrMergeConflict is returned when the merge produces a conflict
	// that MergeBranch has already rolled back.
	ErrMergeConflict = errors.New("host: merge conflict: resolve manually or choose a different approach")

	// ErrReservedBranch is returned when ResolveWorktree is asked to
	// create a worktree for the repository's default branch (e.g.
	// "main"/"master"). The default branch is reserved for the primary
	// checkout — putting it in a separate worktree would prevent
	// `git checkout <default>` from working in the original repo
	// directory and breaks the user's mental model that worktrees are
	// for *other* branches.
	ErrReservedBranch = errors.New("host: cannot create a worktree for the default branch; it is reserved for the primary checkout")

	// ErrInvalidBranchName is returned when ResolveWorktree is given an
	// empty or whitespace-only branch name.
	ErrInvalidBranchName = errors.New("host: branch name must be non-empty")

	// ErrInvalidArgument is returned when a Service method is called with
	// a missing or malformed required argument.
	ErrInvalidArgument = errors.New("host: invalid argument")

	// ErrRepoNotFound is returned when a repo-scoped operation names a
	// slug with no canonical clone on this host (~/work/repos/<slug>).
	// Distinct from ErrNotFound so the mux can emit a repo_not_found
	// code the client can act on (refresh the repo list) vs a missing
	// branch/worktree inside a repo that does exist.
	ErrRepoNotFound = errors.New("host: repo not found")

	// ErrTemplateCloneFailed is returned when CreateProjectFromTemplate
	// can't clone the template repository. The message is deliberately
	// URL-free (clone URLs can embed credentials); the full git error
	// lands in the host's server log only. The mux maps this to a typed
	// template_clone_failed response so clients see an actionable error
	// instead of a masked 502.
	ErrTemplateCloneFailed = errors.New("host: template clone failed")

	// ErrCannotDeleteLocalCheckout is returned when DeleteRepo names a
	// discovered local checkout. The user's own folders are never
	// clank's to delete — only ~/work/repos canonicals are.
	ErrCannotDeleteLocalCheckout = errors.New("host: repo is a checkout owned by the user, not clank; refusing to delete")

	// ErrBranchCheckedOutElsewhere is returned when loading a branch
	// that is checked out in a worktree clank does not manage (a local
	// checkout's primary worktree, or one the user added by hand). Git
	// allows a branch in at most one worktree; fork off it instead.
	ErrBranchCheckedOutElsewhere = errors.New("host: branch is checked out in a worktree clank does not manage; fork off it instead")
)
