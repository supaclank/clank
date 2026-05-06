package host

import (
	"strings"
)

// repoLabelFromURL derives a display label from a git remote URL.
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
func repoLabelFromURL(remoteURL, fallback string) string {
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
