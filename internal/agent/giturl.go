package agent

// Git URL parsing + clone-dir naming. Lives in the agent package because
// GitRef construction and host.Service.workDirFor both need it; pulling
// it into a separate file keeps gitref.go focused on the type itself.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
)

// parseGitURL extracts the host and repo path (without the trailing
// ".git") from any standard git URL form: https://, ssh://, scp-like
// (git@host:owner/repo), or file://.
//
// For file:// URLs the returned host is "file" when the URL has no
// authority (file:///path) and "file://<authority>" when it does
// (file://server/share). Without preserving the authority, two
// distinct remotes like file://a/x and file://b/x would collapse to
// the same identity and clone target.
func parseGitURL(raw string) (host, path string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", fmt.Errorf("empty URL")
	}

	// scp-like form: git@host:owner/repo(.git)
	if !strings.Contains(raw, "://") {
		if at := strings.Index(raw, "@"); at >= 0 {
			rest := raw[at+1:]
			if colon := strings.Index(rest, ":"); colon >= 0 && !strings.Contains(rest[:colon], "/") {
				host = rest[:colon]
				path = strings.TrimSuffix(rest[colon+1:], ".git")
				path = strings.Trim(path, "/")
				if host == "" || path == "" {
					return "", "", fmt.Errorf("scp-like URL missing host or path: %q", raw)
				}
				return host, path, nil
			}
		}
		// Fall through: try to parse as URL by prepending a scheme.
		raw = "https://" + raw
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("parse URL: %w", err)
	}
	if u.Scheme == "file" {
		if u.Path == "" {
			return "", "", fmt.Errorf("file URL %q has no path", raw)
		}
		path = strings.TrimSuffix(u.Path, ".git")
		path = strings.TrimRight(path, "/")
		if path == "" {
			return "", "", fmt.Errorf("file URL %q has empty path after trim", raw)
		}
		host = "file"
		if u.Host != "" {
			// Preserve the authority so file://server/share/repo and
			// file:///share/repo don't collapse to the same identity.
			host = "file://" + strings.ToLower(u.Host)
		}
		return host, path, nil
	}
	if u.Host == "" {
		return "", "", fmt.Errorf("URL %q has no host", raw)
	}
	host = u.Host
	path = strings.TrimSuffix(u.Path, ".git")
	path = strings.Trim(path, "/")
	if path == "" {
		return "", "", fmt.Errorf("URL %q has no repo path", raw)
	}
	return host, path, nil
}

// CloneDirName returns a single filesystem-safe directory name for the
// remote URL. Used by host.Service to pick a stable subdir under
// ClonesDir for each remote.
//
// Format: lowercased "<host>-<path-with-slashes-as-dashes>" with all
// chars outside [a-z0-9._-] replaced by "-", then a short hash of the
// canonical (host, path) pair appended. The hash makes the result
// collision-resistant: two distinct remotes that share the same lossy
// sanitized name (e.g. case differences, escaped chars) get distinct
// clone dirs and cannot accidentally reuse one checkout.
//
// Examples:
//
//	git@github.com:acksell/clank.git    → github.com-acksell-clank-<hash>
//	https://github.com/acksell/clank    → github.com-acksell-clank-<hash>
//	file:///srv/git/foo.git             → file--srv-git-foo-<hash>
func CloneDirName(remoteURL string) (string, error) {
	host, path, err := parseGitURL(remoteURL)
	if err != nil {
		return "", err
	}
	raw := strings.ToLower(host + "-" + strings.ReplaceAll(path, "/", "-"))

	// Allowlist sanitization: anything outside [a-z0-9._-] becomes "-".
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	name := b.String()
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("clone dir name for %q resolves to %q", remoteURL, name)
	}
	if strings.HasPrefix(name, ".") {
		return "", fmt.Errorf("clone dir name for %q starts with dot: %q", remoteURL, name)
	}
	// Append a short hash of the canonical identity so distinct remotes
	// that sanitize to the same name don't share a checkout.
	sum := sha256.Sum256([]byte(host + "\x00" + path))
	return name + "-" + hex.EncodeToString(sum[:6]), nil
}
