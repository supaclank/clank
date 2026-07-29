package agent

import (
	"fmt"
	"strconv"
	"strings"
)

// versionAtLeast reports whether version v is >= floor on
// major.minor.patch. Shared by the per-backend ACP floor gates
// (opencode, hermes). Returns an error when either fails to parse.
func versionAtLeast(v, floor string) (bool, error) {
	cmp, ok := compareVersions(v, floor)
	if !ok {
		return false, fmt.Errorf("unparseable version: %q vs floor %q", v, floor)
	}
	return cmp >= 0, nil
}

// compareVersions returns -1/0/1 for a<b, a==b, a>b on
// major.minor.patch, and ok=false when either fails to parse.
func compareVersions(a, b string) (int, bool) {
	amaj, amin, apat, err := parseVersion(a)
	if err != nil {
		return 0, false
	}
	bmaj, bmin, bpat, err := parseVersion(b)
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

// parseVersion parses "major.minor.patch" with an optional "v" prefix
// and optional pre-release/build suffix on the patch component.
func parseVersion(v string) (major, minor, patch int, err error) {
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
