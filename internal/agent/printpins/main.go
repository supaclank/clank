// Command printpins prints the agent-CLI versions clank is pinned
// against as shell-sourceable KEY=VALUE lines:
//
//	CLAUDE_VERSION=2.1.201
//	OPENCODE_VERSION=1.15.1
//
// The Fly host image build (cmd/clank-host/Dockerfile.fly) runs this
// in its builder stage and installs the CLIs from the output, so the
// baked binaries cannot drift from the code pins — the compatibility
// contract lives in internal/agent and nowhere else.
package main

import (
	"fmt"

	"github.com/acksell/clank/internal/agent"
)

func main() {
	fmt.Printf("CLAUDE_VERSION=%s\n", agent.PinnedClaudeVersion)
	fmt.Printf("OPENCODE_VERSION=%s\n", agent.PinnedOpencodeVersion)
}
