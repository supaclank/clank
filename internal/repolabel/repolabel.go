// Package repolabel derives the owner-independent display label for a
// git repository, used as the `origin_repo` group key on worktree
// listings. Leaf package so any layer can compute the same label
// without cross-imports.
package repolabel

import (
	"path/filepath"
	"strings"

	"github.com/acksell/clank/internal/git"
)

// ComputeRepoLabel returns the owner-independent display label for the
// repo rooted at repoPath. Prefers `git remote get-url origin` so forks
// of the same directory name stay distinguishable; falls back to the
// basename of repoPath when no remote is configured.
//
// The returned value is persisted as `origin_repo` on the worktree row
// (see sync.Worktree.OriginRepo) and consumed by clients (mobile picker,
// TUI sidebar) as the group key when rendering worktrees by repo.
func ComputeRepoLabel(repoPath string) string {
	fallback := filepath.Base(repoPath)
	remoteURL, err := git.RemoteURL(repoPath, "origin")
	if err != nil || remoteURL == "" {
		return fallback
	}
	return RepoLabelFromURL(remoteURL, fallback)
}

// RepoLabelFromURL derives a display label from a git remote URL.
// When the URL contains an owner segment, the label is "owner/repo" so
// forks of the same repo name remain distinguishable in the UI. When
// only a single path segment is present, the bare repo name is returned.
// fallback is used when the URL cannot be parsed into a meaningful name.
//
// Examples:
//
//	"https://github.com/acme/api.git" → "acme/api"
//	"git@github.com:acme/api.git"     → "acme/api"
//	"https://github.com/acme/api"     → "acme/api"
//	"https://example.com/api"         → "api"
func RepoLabelFromURL(remoteURL, fallback string) string {
	u := strings.TrimSuffix(remoteURL, ".git")

	// Extract the path component of the URL (everything after the host).
	var path string
	switch {
	case strings.Contains(u, "://"):
		// scheme://host/path — drop scheme and host.
		rest := u[strings.Index(u, "://")+3:]
		if j := strings.Index(rest, "/"); j != -1 {
			path = rest[j+1:]
		}
	case strings.Index(u, ":") != -1 && !strings.Contains(u[:strings.Index(u, ":")], "/"):
		// SCP-style "user@host:path" — colon separates host from path.
		path = u[strings.Index(u, ":")+1:]
	default:
		path = u
	}

	path = strings.Trim(path, "/")
	if path == "" {
		return fallback
	}

	parts := strings.Split(path, "/")
	repo := strings.TrimSuffix(parts[len(parts)-1], ".git")
	if repo == "" || repo == "." {
		return fallback
	}
	if len(parts) >= 2 {
		if owner := parts[len(parts)-2]; owner != "" {
			return owner + "/" + repo
		}
	}
	return repo
}
