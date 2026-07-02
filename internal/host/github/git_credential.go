package github

// Git credential helper — the always-available auth leg for blobless
// canonical clones. Configured per-repo as
//
//	credential.helper = !"<clank-host>" git-credential
//
// so that lazy blob fetches (and any git command an agent runs against
// origin inside a session) can authenticate at moments clank isn't in
// the loop to inject a token per-command. Reads the token from the
// SAME store the rest of this package uses, at call time — single
// source of truth, so a reconnect/rotation is picked up immediately
// and no secret is ever written into a git config.
//
// Protocol (git-credential(1)): git invokes `<helper> <action>` with
// `key=value` attribute lines on stdin, terminated by a blank line or
// EOF. For `get` we answer with username/password lines; for anything
// else (store/erase) we stay silent — the store is not git's to
// mutate. Printing nothing is the protocol's "no answer": git moves on
// to other helpers or fails auth cleanly under GIT_TERMINAL_PROMPT=0.

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// credentialUsername is GitHub's documented placeholder username for
// token-over-HTTPS Basic auth — same value buildAuthHeader and the
// clone-time inline helper use.
const credentialUsername = "x-access-token"

// GitCredentialHelperValue renders the credential.helper config value
// that routes a repo's auth prompts to this binary. executable is the
// absolute path of the running clank-host (os.Executable()); quoted so
// paths with spaces survive the shell that `!` implies.
func GitCredentialHelperValue(executable string) string {
	return fmt.Sprintf(`!"%s" git-credential`, executable)
}

// RunGitCredentialHelper implements the helper protocol over in/out for
// one invocation. Only `get` for protocol=https host=github.com is
// answered, and only when the store holds a token; every other case
// prints nothing and returns nil so git treats it as "no credential
// here" rather than a helper failure. A store READ error (permission
// fault, corrupt JSON) is returned — that's a real fault worth git's
// stderr, not a silent miss.
func RunGitCredentialHelper(action string, in io.Reader, out io.Writer, store *Store) error {
	if action != "get" {
		return nil
	}
	attrs, err := parseCredentialAttrs(in)
	if err != nil {
		return err
	}
	if !strings.EqualFold(attrs["protocol"], "https") || !strings.EqualFold(attrs["host"], "github.com") {
		return nil
	}
	creds, err := store.Read()
	if err != nil {
		return err
	}
	if creds.AccessToken == "" {
		return nil // not connected — clean no-answer
	}
	if strings.ContainsAny(creds.AccessToken, "\r\n\x00") {
		return fmt.Errorf("git-credential: stored access token contains invalid characters")
	}
	if _, err := fmt.Fprintf(out, "username=%s\npassword=%s\n", credentialUsername, creds.AccessToken); err != nil {
		return fmt.Errorf("write credential response: %w", err)
	}
	return nil
}

// parseCredentialAttrs reads `key=value` lines until a blank line or
// EOF, per git-credential(1). Unknown keys are kept (harmless); a line
// without '=' is a protocol violation worth surfacing.
func parseCredentialAttrs(in io.Reader) (map[string]string, error) {
	attrs := make(map[string]string)
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("git-credential: malformed input line %q", line)
		}
		attrs[key] = value
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("git-credential: read input: %w", err)
	}
	return attrs, nil
}
