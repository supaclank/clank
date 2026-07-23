package agent

import (
	"fmt"
	"strconv"
	"strings"
)

// PinnedOpencodeVersion is the opencode version clank ships against on
// provisioned hosts, and the verified-surface floor for `opencode acp`
// on laptops (the ACP manager refuses older binaries with an upgrade
// hint). Bumping this constant is a deliberate, reviewable change — it
// determines what every fly.io provisioner installs onto a host (and
// what `clank-host print-pins` reports).
//
// Bumping this:
//  1. Update the constant (and re-verify the `opencode acp` surface).
//  2. `make install` — laptops get the new clank that knows the new pin.
//  3. Sprites probe-and-reinstall on next EnsureHost (~30-90s one-shot cost).
//  4. Laptops below the floor see the upgrade hint at first opencode use.
const PinnedOpencodeVersion = "1.17.18"

// OpencodeVersionAtLeast reports whether version v is >= floor. Used by
// the ACP path to gate `opencode acp` on a verified-surface floor.
// Returns an error when either version fails to parse.
func OpencodeVersionAtLeast(v, floor string) (bool, error) {
	cmp, ok := compareOpencodeVersions(v, floor)
	if !ok {
		return false, fmt.Errorf("unparseable opencode version: %q vs floor %q", v, floor)
	}
	return cmp >= 0, nil
}

// compareOpencodeVersions returns -1/0/1 for a<b, a==b, a>b on
// major.minor.patch, and ok=false when either fails to parse.
func compareOpencodeVersions(a, b string) (int, bool) {
	amaj, amin, apat, err := parseOpencodeVersion(a)
	if err != nil {
		return 0, false
	}
	bmaj, bmin, bpat, err := parseOpencodeVersion(b)
	if err != nil {
		return 0, false
	}
	if amaj != bmaj {
		return signum(amaj - bmaj), true
	}
	if amin != bmin {
		return signum(amin - bmin), true
	}
	return signum(apat - bpat), true
}

func signum(x int) int {
	switch {
	case x < 0:
		return -1
	case x > 0:
		return 1
	default:
		return 0
	}
}

// parseOpencodeVersion parses "major.minor.patch" with an optional "v"
// prefix and optional pre-release/build suffix on the patch component.
func parseOpencodeVersion(v string) (major, minor, patch int, err error) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("version %q: want major.minor.patch", v)
	}
	major, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, 0, fmt.Errorf("version %q: major: %w", v, err)
	}
	minor, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, 0, fmt.Errorf("version %q: minor: %w", v, err)
	}
	patchStr := parts[2]
	for i, r := range patchStr {
		if r < '0' || r > '9' {
			patchStr = patchStr[:i]
			break
		}
	}
	patch, err = strconv.Atoi(patchStr)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("version %q: patch: %w", v, err)
	}
	return major, minor, patch, nil
}
