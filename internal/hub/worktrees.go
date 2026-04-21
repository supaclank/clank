package hub

// Hub-side helpers for worktree state. Identity is `(GitRef, branch)`
// post-§7 — repos are no longer keyed by canonical string.

import (
	"github.com/acksell/clank/internal/agent"
)

// markBranchSessionsDone flips every non-archived session attached to
// (ref, branch) to "done". Match is by (Local.Path|Remote.URL,
// WorktreeBranch). Returns the count of sessions updated.
//
// Used by MergeBranchOnHost after a successful merge so that the
// inbox surfaces the work as completed without the user having to
// archive each session by hand.
func (s *Service) markBranchSessionsDone(ref agent.GitRef, branch string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	count := 0
	for _, ms := range s.sessions {
		if ms.info.GitRef.WorktreeBranch != branch {
			continue
		}
		if !sameRepo(ms.info.GitRef, ref) {
			continue
		}
		if ms.info.Visibility == agent.VisibilityArchived || ms.info.Visibility == agent.VisibilityDone {
			continue
		}
		ms.info.Visibility = agent.VisibilityDone
		s.persistSession(ms)
		count++
	}
	return count
}

// sameRepo returns true when a and b name the same repo. Compares
// canonical identity via agent.RepoKey (with branch stripped) so that
// equivalent refs which differ only in surface form — e.g. one carrying
// only a RemoteURL and the other carrying both RemoteURL+LocalPath —
// still match. Branch is ignored — caller checks separately.
func sameRepo(a, b agent.GitRef) bool {
	a.WorktreeBranch = ""
	b.WorktreeBranch = ""
	ka, kb := agent.RepoKey(a), agent.RepoKey(b)
	if ka == "" || kb == "" {
		return false
	}
	return ka == kb
}
