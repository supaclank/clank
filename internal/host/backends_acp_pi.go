package host

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/acksell/clank/internal/agent/acp"
	"github.com/acksell/clank/internal/agent/acptools"
)

// NewPiACPManager serves pi through the pinned pi-acp adapter under
// bun, provisioned lazily into toolsDir alongside the other adapters.
// The adapter spawns the pinned pi CLI via the materialized bun shim
// (PI_ACP_PI_COMMAND — named statically so the supervisor's pre-spawn
// env fingerprint never races provisioning). Credentials are pi's own
// (~/.pi/agent), matching the opencode stance.
func NewPiACPManager(dirs ACPDirs) (*ACPBackendManager, error) {
	if dirs.Tools == "" {
		return nil, fmt.Errorf("pi acp: tools dir is required")
	}
	var paths atomic.Pointer[acptools.Paths]
	profile := acp.PiProfile("", "", acptools.PiWrapperPath(dirs.Tools))
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
		return p.BunBin, []string{p.PiACPEntry}
	}
	return NewACPBackendManager(profile)
}
