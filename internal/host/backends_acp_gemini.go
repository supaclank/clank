package host

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/acksell/clank/internal/agent/acp"
	"github.com/acksell/clank/internal/agent/acptools"
)

// NewGeminiACPManager serves gemini through the pinned @google/gemini-cli
// in its native ACP mode under bun, provisioned lazily into toolsDir
// alongside the other adapters. Credentials are gemini's own (cached
// Google OAuth, or GEMINI_API_KEY riding the parent environment) — no
// env resolver is wired, matching the opencode stance.
func NewGeminiACPManager(dirs ACPDirs) (*ACPBackendManager, error) {
	if dirs.Tools == "" {
		return nil, fmt.Errorf("gemini acp: tools dir is required")
	}
	var paths atomic.Pointer[acptools.Paths]
	profile := acp.GeminiProfile("", "")
	profile.Prepare = func(ctx context.Context, _ string) error {
		p, err := acptools.Ensure(ctx, dirs.Tools)
		if err != nil {
			return err
		}
		paths.Store(&p)
		return nil
	}
	profile.Command = func(string) (string, []string) {
		p := paths.Load() // Prepare ran first (execSpawn ordering)
		return p.BunBin, []string{p.GeminiACPEntry, "--acp"}
	}
	return NewACPBackendManager(profile)
}
