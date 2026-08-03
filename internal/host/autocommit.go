package host

import (
	"fmt"

	"github.com/supaclank/clank/internal/git"
)

// commitAllIfDirty stages everything in the worktree (incl. untracked —
// agents create files) and commits with the push template when
// something actually staged. Shared by PushToRemote and CreatePR so
// both entry points ship uncommitted work the same way. Returns
// whether a commit was made.
func commitAllIfDirty(workdir, branch string) (bool, error) {
	if err := git.AddAll(workdir); err != nil {
		return false, fmt.Errorf("git add -A: %w", err)
	}
	staged, err := git.HasStagedChanges(workdir)
	if err != nil {
		return false, fmt.Errorf("check staged: %w", err)
	}
	if !staged {
		return false, nil
	}
	if err := git.Commit(workdir, pushCommitMessage(branch)); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, nil
}
