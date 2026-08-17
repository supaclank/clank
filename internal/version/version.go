// Package version reports the build version shared by every clank binary.
package version

import (
	"runtime/debug"
	"sync"
)

// Version is stamped by the release pipeline via
// -ldflags "-X github.com/supaclank/clank/internal/version.Version=v1.2.3".
// Empty for `go build`/`go install` builds, which report Go's own VCS
// stamp instead (see String).
var Version string

// Unknown is what String reports when neither ldflags nor Go's build
// info carry a version (e.g. binaries built with -buildvcs=false outside
// a module).
const Unknown = "unknown"

// String returns the ldflags-stamped Version, or the main-module version
// Go recorded at build time (a tag or pseudo-version, "+<rev>" appended
// when a VCS revision is known), or Unknown.
var String = sync.OnceValue(func() string {
	if Version != "" {
		return Version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return Unknown
	}
	return fromBuildInfo(info)
})

func fromBuildInfo(info *debug.BuildInfo) string {
	v := info.Main.Version
	if v == "" {
		v = Unknown
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			rev := s.Value
			if len(rev) > 12 {
				rev = rev[:12]
			}
			return v + "+" + rev
		}
	}
	return v
}
