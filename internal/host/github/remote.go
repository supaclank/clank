package github

import (
	"errors"
	"strings"
)

// ErrNotGitHubRemote is returned by ParseGitHubRemote when the URL
// doesn't point to github.com. v1 supports github.com only; GHE and
// non-GitHub forges are deferred.
var ErrNotGitHubRemote = errors.New("not a github.com remote")

// ParseGitHubRemote extracts owner and repo from a github.com remote
// URL. Accepts the three forms git uses in practice:
//
//	https://github.com/owner/repo(.git)?
//	git@github.com:owner/repo(.git)?
//	ssh://git@github.com/owner/repo(.git)?
//
// Anything else returns ErrNotGitHubRemote, including non-github.com
// hosts and URLs that don't have both an owner and a repo segment.
func ParseGitHubRemote(remoteURL string) (owner, repo string, err error) {
	u := strings.TrimSuffix(strings.TrimSpace(remoteURL), ".git")
	if u == "" {
		return "", "", ErrNotGitHubRemote
	}

	var host, path string
	if rest, ok := strings.CutPrefix(u, "https://"); ok {
		host, path = splitURLAuthority(rest)
	} else if rest, ok := strings.CutPrefix(u, "http://"); ok {
		host, path = splitURLAuthority(rest)
	} else if rest, ok := strings.CutPrefix(u, "ssh://"); ok {
		host, path = splitURLAuthority(rest)
	} else if prefix, rest, ok := strings.Cut(u, ":"); ok && !strings.Contains(prefix, "/") {
		// SCP-style "user@host:path"
		path = rest
		if _, h, ok := strings.Cut(prefix, "@"); ok {
			host = h
		} else {
			host = prefix
		}
	} else {
		return "", "", ErrNotGitHubRemote
	}

	if host != "github.com" {
		return "", "", ErrNotGitHubRemote
	}
	path = strings.Trim(path, "/")
	parts := strings.Split(path, "/")
	// Strictly require `owner/repo` — refuse anything with extra
	// segments (e.g. github.com/owner/repo/tree/branch). The truncate-
	// to-first-two behavior would silently resolve to the wrong repo
	// when a user pasted a tree URL instead of a clone URL.
	if len(parts) != 2 {
		return "", "", ErrNotGitHubRemote
	}
	owner = parts[0]
	repo = strings.TrimSuffix(parts[1], ".git")
	if owner == "" || repo == "" {
		return "", "", ErrNotGitHubRemote
	}
	return owner, repo, nil
}

// splitURLAuthority parses "[user@]host/path" into host and path,
// dropping the optional user@ prefix.
func splitURLAuthority(s string) (host, path string) {
	if _, h, ok := strings.Cut(s, "@"); ok {
		s = h
	}
	host, path, _ = strings.Cut(s, "/")
	return host, path
}

// RemoteHost returns just the host portion of a git remote URL,
// regardless of whether the host is github.com. Used by the PR
// preview endpoint to surface the actual host in "non_github"
// error messages (e.g. "Origin points to gitlab.com").
//
// Returns the empty string when the URL is unparseable. Accepts the
// same three URL shapes as ParseGitHubRemote.
func RemoteHost(remoteURL string) string {
	u := strings.TrimSuffix(strings.TrimSpace(remoteURL), ".git")
	if u == "" {
		return ""
	}
	var host string
	if rest, ok := strings.CutPrefix(u, "https://"); ok {
		host, _ = splitURLAuthority(rest)
	} else if rest, ok := strings.CutPrefix(u, "http://"); ok {
		host, _ = splitURLAuthority(rest)
	} else if rest, ok := strings.CutPrefix(u, "ssh://"); ok {
		host, _ = splitURLAuthority(rest)
	} else if prefix, _, ok := strings.Cut(u, ":"); ok && !strings.Contains(prefix, "/") {
		if _, h, ok := strings.Cut(prefix, "@"); ok {
			host = h
		} else {
			host = prefix
		}
	}
	return host
}
