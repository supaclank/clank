package host

import "errors"

// Sentinel errors for Service methods. Callers (e.g. HTTP handlers in
// host/mux) use errors.Is to translate these into appropriate status
// codes without coupling to string matching.
var (
	// ErrNotFound is returned when a requested repo, branch, or worktree
	// does not exist on the host.
	ErrNotFound = errors.New("host: not found")

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
)
