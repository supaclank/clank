package git

import (
	"errors"
	"fmt"
	"strings"
)

// ErrMergeConflict is returned by Merge when the merge stops on conflicts.
// The repository is left mid-merge (MERGE_HEAD present) so the caller can
// inspect ConflictedFiles, hand resolution to an agent, or AbortMerge.
var ErrMergeConflict = errors.New("git merge: conflicts")

// ConflictedFiles returns the working-tree paths with unresolved merge
// conflicts (the "U" entries of `git diff --diff-filter=U`). Empty when
// there are none.
func ConflictedFiles(dir string) ([]string, error) {
	out, err := gitCmd(dir, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, fmt.Errorf("list conflicted files: %w", err)
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

// ResetHard moves the current branch and working tree to ref, discarding
// local commits and uncommitted changes not reachable from ref.
// Destructive — callers should BackupRef the prior HEAD first when the
// discarded work might be wanted back.
func ResetHard(dir, ref string) error {
	if _, err := gitCmd(dir, "reset", "--hard", ref); err != nil {
		return fmt.Errorf("reset --hard %s: %w", ref, err)
	}
	return nil
}

// MergeFF fast-forwards the current branch to ref. Fails rather than
// creating a merge commit when the branch can't be fast-forwarded (local
// has commits ref lacks). Gate with IsAncestor(HEAD, ref) first.
func MergeFF(dir, ref string) error {
	if _, err := gitCmd(dir, "merge", "--ff-only", ref); err != nil {
		return fmt.Errorf("merge --ff-only %s: %w", ref, err)
	}
	return nil
}

// Merge merges ref into the current branch — fast-forwarding when possible,
// otherwise creating a merge commit with message. Returns ErrMergeConflict
// when the merge stops on conflicts; the repo is left mid-merge for the
// caller to resolve (ConflictedFiles) or AbortMerge.
func Merge(dir, ref, message string) error {
	if _, err := gitCmd(dir, "merge", "-m", message, ref); err != nil {
		if IsMerging(dir) {
			return ErrMergeConflict
		}
		return fmt.Errorf("merge %s: %w", ref, err)
	}
	return nil
}

// BackupRef points the fully-qualified ref name at target (typically HEAD
// before a destructive reset), keeping discarded work recoverable. name
// must be fully qualified, e.g. "refs/clank/backup/<branch>-<sha>".
func BackupRef(dir, name, target string) error {
	if _, err := gitCmd(dir, "update-ref", name, target); err != nil {
		return fmt.Errorf("update-ref %s -> %s: %w", name, target, err)
	}
	return nil
}
