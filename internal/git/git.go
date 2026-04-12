// Package git provides helpers for interacting with git repositories,
// focused on worktree and branch management for Clank's session isolation.
//
// All functions shell out to the git CLI rather than using a Go git library.
// This keeps the dependency footprint small and ensures exact behavioral
// parity with the user's installed git version.
package git

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Worktree represents a git worktree entry as returned by `git worktree list --porcelain`.
type Worktree struct {
	Path   string // Absolute filesystem path of the worktree
	Branch string // Branch checked out in this worktree (short name, e.g. "main")
	Bare   bool   // True if this is the bare repository entry
	Head   string // HEAD commit hash
}

// RepoRoot returns the top-level directory of the git repository containing dir.
func RepoRoot(dir string) (string, error) {
	out, err := gitCmd(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("not a git repository (or any parent): %w", err)
	}
	return strings.TrimSpace(out), nil
}

// CurrentBranch returns the currently checked-out branch in dir.
// Returns "HEAD" if in detached HEAD state.
func CurrentBranch(dir string) (string, error) {
	out, err := gitCmd(dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", fmt.Errorf("get current branch: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// DefaultBranch returns the default branch name for the repository containing dir.
// It checks for refs/heads/main first, then refs/heads/master, then falls back
// to whatever HEAD points to on origin, and finally returns "main" as a last resort.
func DefaultBranch(dir string) (string, error) {
	// Check if "main" exists locally.
	if _, err := gitCmd(dir, "rev-parse", "--verify", "refs/heads/main"); err == nil {
		return "main", nil
	}
	// Check if "master" exists locally.
	if _, err := gitCmd(dir, "rev-parse", "--verify", "refs/heads/master"); err == nil {
		return "master", nil
	}
	// Try origin HEAD.
	out, err := gitCmd(dir, "symbolic-ref", "refs/remotes/origin/HEAD")
	if err == nil {
		ref := strings.TrimSpace(out)
		// ref is like "refs/remotes/origin/main"
		parts := strings.Split(ref, "/")
		if len(parts) > 0 {
			return parts[len(parts)-1], nil
		}
	}
	return "main", nil
}

// LocalBranches returns the list of local branch names in the repository containing dir.
func LocalBranches(dir string) ([]string, error) {
	out, err := gitCmd(dir, "branch", "--format=%(refname:short)")
	if err != nil {
		return nil, fmt.Errorf("list local branches: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var branches []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			branches = append(branches, line)
		}
	}
	return branches, nil
}

// ListWorktrees returns all worktrees for the repository containing dir.
func ListWorktrees(dir string) ([]Worktree, error) {
	out, err := gitCmd(dir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("list worktrees: %w", err)
	}
	return parseWorktreeList(out), nil
}

// parseWorktreeList parses the porcelain output of `git worktree list --porcelain`.
//
// Format is blocks separated by blank lines:
//
//	worktree /path/to/worktree
//	HEAD abc123
//	branch refs/heads/main
//
//	worktree /path/to/other
//	HEAD def456
//	branch refs/heads/feature
func parseWorktreeList(output string) []Worktree {
	var worktrees []Worktree
	blocks := strings.Split(strings.TrimSpace(output), "\n\n")
	for _, block := range blocks {
		if block == "" {
			continue
		}
		var wt Worktree
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(line, "worktree "):
				wt.Path = strings.TrimPrefix(line, "worktree ")
			case strings.HasPrefix(line, "HEAD "):
				wt.Head = strings.TrimPrefix(line, "HEAD ")
			case strings.HasPrefix(line, "branch "):
				ref := strings.TrimPrefix(line, "branch ")
				// Convert refs/heads/foo to foo
				wt.Branch = strings.TrimPrefix(ref, "refs/heads/")
			case line == "bare":
				wt.Bare = true
			}
		}
		if wt.Path != "" {
			worktrees = append(worktrees, wt)
		}
	}
	return worktrees
}

// FindWorktreeForBranch returns the worktree that has the given branch checked out,
// or nil if no worktree exists for that branch.
func FindWorktreeForBranch(dir, branch string) (*Worktree, error) {
	worktrees, err := ListWorktrees(dir)
	if err != nil {
		return nil, err
	}
	for _, wt := range worktrees {
		if wt.Branch == branch {
			return &wt, nil
		}
	}
	return nil, nil
}

// BranchExists returns true if the given branch exists locally.
func BranchExists(dir, branch string) (bool, error) {
	_, err := gitCmd(dir, "rev-parse", "--verify", "refs/heads/"+branch)
	if err != nil {
		// git rev-parse exits non-zero if the ref doesn't exist.
		return false, nil
	}
	return true, nil
}

// WorktreeDir returns the conventional Clank worktree directory path for a branch.
// Convention: ~/.clank/worktrees/<project-name>/<sanitized-branch>/
func WorktreeDir(projectName, branch string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	sanitized := sanitizeBranchName(branch)
	return filepath.Join(home, ".clank", "worktrees", projectName, sanitized), nil
}

// AddWorktree creates a new worktree for an existing branch.
func AddWorktree(repoDir, worktreeDir, branch string) error {
	_, err := gitCmd(repoDir, "worktree", "add", worktreeDir, branch)
	if err != nil {
		return fmt.Errorf("add worktree for branch %q at %s: %w", branch, worktreeDir, err)
	}
	return nil
}

// AddWorktreeNewBranch creates a new worktree with a new branch based on the given base ref.
func AddWorktreeNewBranch(repoDir, worktreeDir, branch, base string) error {
	_, err := gitCmd(repoDir, "worktree", "add", "-b", branch, worktreeDir, base)
	if err != nil {
		return fmt.Errorf("add worktree for new branch %q (base %s) at %s: %w", branch, base, worktreeDir, err)
	}
	return nil
}

// RemoveWorktree removes a worktree. If force is true, uses --force to remove
// even if the worktree has uncommitted changes.
func RemoveWorktree(repoDir, worktreeDir string, force bool) error {
	args := []string{"worktree", "remove", worktreeDir}
	if force {
		args = append(args, "--force")
	}
	_, err := gitCmd(repoDir, args...)
	if err != nil {
		return fmt.Errorf("remove worktree %s: %w", worktreeDir, err)
	}
	return nil
}

// DiffStat returns the total lines added and removed in a worktree compared
// to the given base branch. This includes both staged and unstaged changes.
// The base is typically the repo's default branch (e.g. "main").
func DiffStat(worktreeDir, base string) (added, removed int, err error) {
	// Use merge-base to diff against the point where the branch diverged,
	// not the current tip of the base branch. This shows only the work
	// done on this branch, not unrelated commits on main.
	mergeBase, mbErr := gitCmd(worktreeDir, "merge-base", base, "HEAD")
	diffTarget := base
	if mbErr == nil {
		diffTarget = strings.TrimSpace(mergeBase)
	}

	// --numstat gives machine-parseable "added\tremoved\tfile" lines.
	out, err := gitCmd(worktreeDir, "diff", "--numstat", diffTarget)
	if err != nil {
		return 0, 0, fmt.Errorf("diff stat against %s: %w", base, err)
	}
	added, removed = parseNumstat(out)

	// Also include uncommitted (staged + unstaged) changes on top.
	uncommitted, err := gitCmd(worktreeDir, "diff", "--numstat", "HEAD")
	if err == nil {
		a, r := parseNumstat(uncommitted)
		added += a
		removed += r
	}

	return added, removed, nil
}

// parseNumstat parses `git diff --numstat` output and returns totals.
// Each line is "added\tremoved\tfilename". Binary files show "-\t-\t..." and are skipped.
func parseNumstat(output string) (added, removed int) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 2 || parts[0] == "-" {
			continue // binary file
		}
		a, _ := strconv.Atoi(parts[0])
		r, _ := strconv.Atoi(parts[1])
		added += a
		removed += r
	}
	return added, removed
}

// sanitizeBranchName replaces slashes and other filesystem-unfriendly characters
// with dashes for use as directory names.
func sanitizeBranchName(branch string) string {
	r := strings.NewReplacer(
		"/", "-",
		"\\", "-",
		":", "-",
		" ", "-",
	)
	return r.Replace(branch)
}

// gitCmd runs a git command in the given directory and returns its stdout.
func gitCmd(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %s (%w)", strings.Join(args, " "), strings.TrimSpace(stderr.String()), err)
	}
	return stdout.String(), nil
}
