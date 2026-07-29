package host

import (
	"context"
	"fmt"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/agent/acp"
)

// NewHermesACPManager serves hermes through `hermes acp` on the user's
// own install. Prepare gates on the verified-surface floor (retried
// until it passes, mirroring opencode). No guidance channel and no env
// resolver: hermes authenticates itself via its own config and stores.
func NewHermesACPManager() (*ACPBackendManager, error) {
	profile := acp.HermesProfile("hermes")
	var floor onceUntilSuccess
	profile.Prepare = func(ctx context.Context, _ string) error {
		return floor.do(func() error {
			v, err := agent.HermesACPVersion(ctx)
			if err != nil {
				return fmt.Errorf("probe hermes acp version: %w", err)
			}
			ok, err := agent.HermesVersionAtLeast(v, agent.PinnedHermesVersion)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("hermes %s is older than the verified ACP floor %s — upgrade hermes-agent", v, agent.PinnedHermesVersion)
			}
			return nil
		})
	}
	return NewACPBackendManager(profile)
}
