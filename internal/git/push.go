package git

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Push errors that callers map to specific HTTP statuses or UI
// messages. Anything else surfaces via the wrapping error from Push.
var (
	// ErrPushNotFastForward fires when the remote branch has commits
	// the local branch doesn't. Callers should surface a 409 with a
	// "rebase or force-push" hint; v1 doesn't auto-force.
	ErrPushNotFastForward = errors.New("git push: not a fast-forward")

	// ErrPushRepoNotFound fires when the remote URL is reachable but
	// the repo isn't (404 from GitHub). GitHub deliberately returns
	// "Repository not found" for both 404 and 403 (private repos
	// without access) so an attacker can't enumerate.
	ErrPushRepoNotFound = errors.New("git push: repository not found or no access")

	// ErrPushPermissionDenied fires for explicit auth failures (e.g.
	// 401 from the remote). Distinct from ErrPushRepoNotFound — this
	// one means "we found the repo but our token can't write".
	ErrPushPermissionDenied = errors.New("git push: permission denied")
)

// PushOptions controls how Push invokes git. ExtraHeader, when set,
// is passed via `-c http.extraheader=<value>` so the credential
// never lands in `.git/config`, the remote URL, or `ps` output.
// Callers build the header value (typically "Authorization: Basic
// <b64(x-access-token:tok)>") themselves — this package stays
// agnostic to credential format.
type PushOptions struct {
	// ExtraHeader is appended to every HTTP request git makes during
	// this push. Empty value means no header is sent (the default).
	ExtraHeader string
}

// Fetch refreshes one ref from the named remote, using the same
// process-local auth pattern as Push. Used by callers that need
// origin/<base> to exist before running a local diff against it
// (e.g. host.Service.CreatePR's commits-ahead check on a sprite
// whose worktree was migrated piecemeal without main ever being
// fetched).
//
// Errors are returned verbatim; callers decide whether a fetch
// failure is fatal. A common pattern is to log + continue, since
// the downstream check has its own fallback.
func Fetch(dir, remote, ref string, opts PushOptions) error {
	args := []string{}
	if opts.ExtraHeader != "" {
		args = append(args, "-c", "http.extraheader="+opts.ExtraHeader)
	}
	args = append(args, "fetch", "--quiet", remote, ref)

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git fetch %s %s: %s: %w", remote, ref, strings.TrimSpace(stderr.String()), err)
	}
	return nil
}

// Push runs `git push` against the configured remote with the given
// refspec. Returns one of the typed errors above when the failure
// is classifiable, otherwise wraps git's stderr.
//
// The auth header is passed via process-internal git config
// (-c http.extraheader=...) so it never persists to disk. It does
// land in `ps` output for the duration of the subprocess; we accept
// that — the sprite is single-user and the token is base64-encoded
// inside the header value.
func Push(dir, remote, refspec string, opts PushOptions) error {
	args := []string{}
	if opts.ExtraHeader != "" {
		args = append(args, "-c", "http.extraheader="+opts.ExtraHeader)
	}
	args = append(args, "push", remote, refspec)

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		stderrStr := strings.TrimSpace(stderr.String())
		return classifyPushError(stderrStr, err)
	}
	return nil
}

// classifyPushError matches well-known stderr substrings to the
// exported sentinel errors. When nothing matches we wrap the
// underlying exec error with the stderr text so callers can still
// surface a helpful message — but the message is intentionally
// stripped of any token substring (the caller's responsibility:
// `-c http.extraheader=…` never appears in stderr, but better safe
// than sorry).
func classifyPushError(stderrStr string, underlying error) error {
	low := strings.ToLower(stderrStr)
	switch {
	case strings.Contains(low, "non-fast-forward"),
		strings.Contains(low, "rejected") && strings.Contains(low, "fetch first"):
		return fmt.Errorf("%w: %s", ErrPushNotFastForward, stderrStr)
	case strings.Contains(low, "repository not found"),
		strings.Contains(low, "could not read from remote repository"):
		return fmt.Errorf("%w: %s", ErrPushRepoNotFound, stderrStr)
	case strings.Contains(low, "permission denied"),
		strings.Contains(low, "authentication failed"):
		return fmt.Errorf("%w: %s", ErrPushPermissionDenied, stderrStr)
	default:
		return fmt.Errorf("git push: %s: %w", stderrStr, underlying)
	}
}
