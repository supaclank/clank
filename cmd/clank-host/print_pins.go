package main

// `clank-host print-pins` — prints the toolchain versions this binary
// is pinned against, as shell-sourceable KEY=VALUE lines:
//
//	CLAUDE_VERSION=2.1.201
//	OPENCODE_VERSION=1.15.1
//	BUN_VERSION=1.3.14
//
// The host-image build asks the binary what it needs, then installs
// exactly those — so the baked CLIs cannot drift from the code pins,
// and an operator packaging clank-host into their own image (from any
// registry) gets the same guarantee without depending on clank's
// Dockerfile. The compat contract lives in internal/agent and travels
// inside the binary.

import (
	"fmt"
	"io"
	"os"

	"github.com/acksell/clank/internal/agent"
)

// printPinsCommand is the argv[1] that selects this subcommand.
const printPinsCommand = "print-pins"

// runPrintPins writes the pinned versions to stdout and returns the
// process exit code.
func runPrintPins() int {
	if err := printPins(os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "print-pins: %v\n", err)
		return 1
	}
	return 0
}

// printPins writes the pinned toolchain versions as shell-sourceable
// KEY=VALUE lines. Split from runPrintPins so the format is unit-testable.
func printPins(w io.Writer) error {
	if _, err := fmt.Fprintf(w, "CLAUDE_VERSION=%s\n", agent.PinnedClaudeVersion); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "OPENCODE_VERSION=%s\n", agent.PinnedOpencodeVersion); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "BUN_VERSION=%s\n", agent.PinnedBunVersion); err != nil {
		return err
	}
	return nil
}
