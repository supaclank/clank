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
			return formatOpencodeIncompatibleHint(typed, agent.PinnedOpencodeVersion)
		}
		return err
	}
	if warn != nil {
		fmt.Fprintf(stderr, "  warning: %s\n", warn.String())
	}
	return nil
}

// formatOpencodeIncompatibleHint composes the user-facing recovery
// hint for a hard version mismatch. The wording is conditioned on
// which side has drifted from the pin: telling the user to "upgrade
// your laptop" is misleading when the laptop is already ahead of
// the pin (in that case clank itself is the lagging side and the
// pin needs to be bumped).
func formatOpencodeIncompatibleHint(e *agent.OpencodeIncompatibleError, pin string) error {
	switch agent.DiagnoseOpencodeMismatch(e.Local, e.Remote, pin) {
	case agent.OpencodeMismatchLaptopBehindPin:
		return fmt.Errorf(
			"%s\n\nclank pins opencode at version %s, but your laptop is on %s. Upgrade your laptop to match:\n  opencode upgrade v%s",
			e.Error(), pin, e.Local, pin,
		)
	case agent.OpencodeMismatchLaptopAheadOfPin:
		return fmt.Errorf(
			"%s\n\nclank pins opencode at version %s, but your laptop is on %s (newer). This usually means clank itself is behind — update clank so its pin matches the opencode you're running, or downgrade your laptop:\n  opencode upgrade v%s",
			e.Error(), pin, e.Local, pin,
		)
	case agent.OpencodeMismatchSpriteDrifted:
		return fmt.Errorf(
			"%s\n\nclank pins opencode at version %s and your laptop matches, but the remote sprite is on %s. Restart the sprite via your remote provisioner so EnsureHost reinstalls the pin, then retry",
			e.Error(), pin, e.Remote,
		)
	case agent.OpencodeMismatchBothDrifted:
		return fmt.Errorf(
			"%s\n\nclank pins opencode at version %s, but neither side matches (laptop=%s, sprite=%s). Bring at least one side to the pin (opencode upgrade v%s on the laptop, or restart the sprite) before retrying",
			e.Error(), pin, e.Local, e.Remote, pin,
		)
	default:
		return fmt.Errorf(
			"%s\n\nclank pins opencode at version %s. Bring both sides to the pin, then retry",
			e.Error(), pin,
		)
	}
}
