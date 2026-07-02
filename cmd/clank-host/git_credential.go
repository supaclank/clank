package main

// `clank-host git-credential <action>` — the git credential helper
// subcommand. Dispatched from main() before flag parsing (the serve
// flags don't apply). Kept to a thin shim: resolve the home dir, hand
// stdin/stdout to the protocol implementation in internal/host/github.

import (
	"fmt"
	"os"

	githubpkg "github.com/acksell/clank/internal/host/github"
)

// gitCredentialCommand is the argv[1] that selects this subcommand.
const gitCredentialCommand = "git-credential"

// runGitCredential executes one helper invocation and returns the
// process exit code. args are argv[2:]; git always passes exactly one
// action (get/store/erase). A missing action is treated as a no-op
// rather than an error — helpers are expected to be forgiving.
func runGitCredential(args []string) int {
	action := ""
	if len(args) > 0 {
		action = args[0]
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "clank-host git-credential:", err)
		return 1
	}
	if err := githubpkg.RunGitCredentialHelper(action, os.Stdin, os.Stdout, githubpkg.NewStore(home)); err != nil {
		fmt.Fprintln(os.Stderr, "clank-host git-credential:", err)
		return 1
	}
	return 0
}
