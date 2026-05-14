package agent

import (
	"fmt"
	"strconv"
	"strings"
)

// PinnedOpencodeVersion is the opencode version clank ships against.
// Bumping this constant is a deliberate, reviewable change — it
// determines what every fly.io / daytona / local provisioner
// installs onto a sprite, AND what the laptop-side error message
// suggests when versions drift.
//
// Why pin: opencode's export/import schema is forward-incompatible
// across minor versions (we lived through the 1.3 → 1.14 diff
// schema rewrite the hard way). A pin makes "everyone runs the
// same opencode" the default state instead of "everyone runs
// whatever was latest when their sprite was provisioned."
//
// Bumping this:
//   1. Update the constant.
//   2. `make install` — laptops get the new clank that knows the new pin.
//   3. Sprites probe-and-reinstall on next EnsureHost (~30-90s one-shot cost).
//   4. Laptops `opencode upgrade` — runtime check refuses migrations
//      until they do.
const PinnedOpencodeVersion = "1.14.49"

// OpencodeIncompatibleError is returned by AssertOpencodeVersionsCompatible
// when local and remote opencode versions can't safely round-trip session
// blobs. Callers (push.go / pull.go) format this for the user with the
// upgrade-instruction tail.
type OpencodeIncompatibleError struct {
	Local, Remote string
	Reason        string
}

func (e *OpencodeIncompatibleError) Error() string {
	return fmt.Sprintf("opencode version mismatch (local=%s, remote=%s): %s", e.Local, e.Remote, e.Reason)
}

// AssertOpencodeVersionsCompatible enforces clank's session-sync
// version policy:
//
//   - Same major.minor: OK (silent if exactly equal, returns a
//     non-nil *OpencodeWarning when only patch differs).
//   - Different minor (same major): refuse — opencode 1.3 → 1.14
//     redesigned the diff schema, so any minor bump may break
//     export/import round-trips.
//   - Different major: refuse — bigger blast radius.
//   - Either side empty: refuse — we can't reason about compat
//     without both versions.
//
// Returns:
//
//   - (nil, nil)               — exact match, all good
//   - (*OpencodeWarning, nil)  — patch-only diff, caller should log
//   - (nil, error)             — incompatible; refuse the migration
//
// The two-output shape lets callers distinguish "warn the user" from
// "abort the operation" without comparing strings.
func AssertOpencodeVersionsCompatible(local, remote string) (*OpencodeWarning, error) {
	if local == "" || remote == "" {
		return nil, &OpencodeIncompatibleError{
			Local: local, Remote: remote,
			Reason: "could not determine one or both opencode versions",
		}
	}
	lMaj, lMin, lPat, err := parseOpencodeVersion(local)
	if err != nil {
		return nil, &OpencodeIncompatibleError{
			Local: local, Remote: remote,
			Reason: fmt.Sprintf("local version unparseable: %v", err),
		}
	}
	rMaj, rMin, rPat, err := parseOpencodeVersion(remote)
	if err != nil {
		return nil, &OpencodeIncompatibleError{
			Local: local, Remote: remote,
			Reason: fmt.Sprintf("remote version unparseable: %v", err),
		}
	}
	if lMaj == rMaj && lMin == rMin && lPat == rPat {
		// Semantic equality: "1.14" and "1.14.0" both parse to (1,14,0).
		return nil, nil
	}
	if lMaj != rMaj {
		return nil, &OpencodeIncompatibleError{
			Local: local, Remote: remote,
			Reason: "major version differs — major bumps usually carry breaking changes; upgrade one side to match the other before retrying",
		}
	}
	if lMin != rMin {
		// Specifically tested in production: 1.3.x → 1.14.x broke
		// session round-trips because opencode redesigned the diff
		// schema between minors. Refuse.
		return nil, &OpencodeIncompatibleError{
			Local: local, Remote: remote,
			Reason: "minor version differs — opencode has shipped breaking schema changes between minors before; upgrade one side to match the other before retrying",
		}
	}
	return &OpencodeWarning{
		Local: local, Remote: remote,
		Message: "opencode patch versions differ across hosts — usually fine, but consider upgrading the older side if you see schema errors",
	}, nil
}

// OpencodeWarning is the non-fatal counterpart to
// OpencodeIncompatibleError. Callers should log it but proceed.
type OpencodeWarning struct {
	Local, Remote string
	Message       string
}

func (w *OpencodeWarning) String() string {
	return fmt.Sprintf("opencode version drift (local=%s, remote=%s): %s", w.Local, w.Remote, w.Message)
}

// parseOpencodeVersion accepts "1.14.48" and returns (1, 14, 48).
// opencode doesn't ship prerelease tags or build metadata as of
// 1.x, so the parser is intentionally simple. If opencode adopts
// "-rc.1" style suffixes later, this function needs to grow.
func parseOpencodeVersion(v string) (major, minor, patch int, err error) {
	v = strings.TrimSpace(v)
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return 0, 0, 0, fmt.Errorf("expected major.minor[.patch], got %q", v)
	}
	major, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, 0, fmt.Errorf("parse major %q: %w", parts[0], err)
	}
	minor, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, 0, fmt.Errorf("parse minor %q: %w", parts[1], err)
	}
	if len(parts) == 3 {
		patch, err = strconv.Atoi(parts[2])
		if err != nil {
			return 0, 0, 0, fmt.Errorf("parse patch %q: %w", parts[2], err)
		}
	}
	return major, minor, patch, nil
}
