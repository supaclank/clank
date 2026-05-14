package clankcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/acksell/clank/internal/agent"
	daemonclient "github.com/acksell/clank/internal/daemonclient"
)

// Asymmetric timeouts for the version compatibility check. The two
// hosts have very different latency floors:
//
//   - Local clank-host is a Unix-socket call to an always-warm
//     process that serves /software-manifest from in-memory cache.
//     Should answer in milliseconds. A short timeout makes a hung
//     local daemon fail fast.
//
//   - Remote clank-host (via the cloud gateway) may need to
//     cold-start: EnsureHost wakes the sprite, installs the
//     embedded clank-host binary if missing, installs opencode at
//     the pinned version if missing. That's 30-90s on a truly
//     fresh sprite, and the previous symmetric 10s timeout
//     produced spurious "context deadline exceeded" errors on
//     first push.
const (
	localCompatCheckTimeout  = 5 * time.Second
	remoteCompatCheckTimeout = 2 * time.Minute
)

// assertOpencodeCompatible queries each end's software manifest
// and enforces the version-skew policy in
// agent.AssertOpencodeVersionsCompatible:
//
//   - exact match: silent OK
//   - patch-only diff: log a one-line warning to stderr, proceed
//   - minor or major diff: return an error so the caller aborts
//     the migration before any code/session work begins
//
// Failures fetching either manifest are reported as compatibility
// errors with the upgrade hint — better to refuse than guess.
//
// Local and remote queries use independent budgets (see
// localCompatCheckTimeout / remoteCompatCheckTimeout) so a slow
// sprite cold-start doesn't make a fast local probe look like a
// failure.
func assertOpencodeCompatible(ctx context.Context, stderr io.Writer, local, remote *daemonclient.Client) error {
	localCtx, cancelLocal := context.WithTimeout(ctx, localCompatCheckTimeout)
	defer cancelLocal()
	localManifest, err := local.SoftwareManifest(localCtx)
	if err != nil {
		return fmt.Errorf("read laptop software manifest: %w", err)
	}

	remoteCtx, cancelRemote := context.WithTimeout(ctx, remoteCompatCheckTimeout)
	defer cancelRemote()
	remoteManifest, err := remote.SoftwareManifest(remoteCtx)
	if err != nil {
		return fmt.Errorf("read remote software manifest (sprite may still be cold-starting; retry in a moment): %w", err)
	}

	warn, err := agent.AssertOpencodeVersionsCompatible(localManifest.OpenCode.Version, remoteManifest.OpenCode.Version)
	if err != nil {
		var typed *agent.OpencodeIncompatibleError
		if errors.As(err, &typed) {
			// Surface the clank-pinned version explicitly so the
			// user knows exactly which version to land on, not just
			// "match the other side." If the laptop is the drifted
			// side (the common case — sprites get the pin
			// automatically on EnsureHost), suggesting the pin
			// directly avoids a guessing game.
			return fmt.Errorf(
				"%s\n\nclank pins opencode at version %s. Upgrade your laptop to match:\n  opencode upgrade v%s\n\n(The sprite re-installs the pinned opencode automatically on its next EnsureHost; if it's the side that's drifted, restart the sprite via your remote provisioner and retry.)",
				typed.Error(), agent.PinnedOpencodeVersion, agent.PinnedOpencodeVersion,
			)
		}
		return err
	}
	if warn != nil {
		fmt.Fprintf(stderr, "  warning: %s\n", warn.String())
	}
	return nil
}
