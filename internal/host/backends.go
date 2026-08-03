package host

// Shared backend-manager helpers. The bespoke per-backend managers that
// used to live here were retired by the ACP migration (M2/M3); the
// generic ACP manager is in backends_acp.go.

import (
	"log"

	"github.com/supaclank/clank/internal/agent/guidance"
)

// installGuidanceSkills materializes the stack playbook the system prompt
// points at (guidance.InstallSkills) into ~/.claude/skills. Runs for fresh
// AND resumed sessions: the prompt already in a resumed session's history
// references the skill by path, and this refreshes stale copies after a
// clank upgrade. Non-fatal — a session without the playbook still has the
// distilled prompt. Fire-and-forget: the agent doesn't need the skill files
// at the exact millisecond CreateBackend returns, so this runs off the
// session-creation request path instead of blocking it.
func installGuidanceSkills(workDir string) {
	go func() {
		if err := guidance.InstallSkills(workDir); err != nil {
			log.Printf("guidance: install skills in %s: %v", workDir, err)
		}
	}()
}
