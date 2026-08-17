package version

import (
	"runtime/debug"
	"testing"
)

func TestFromBuildInfo(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		info *debug.BuildInfo
		want string
	}{
		{"empty", &debug.BuildInfo{}, Unknown},
		{"tag only", &debug.BuildInfo{Main: debug.Module{Version: "v0.3.0"}}, "v0.3.0"},
		{"tag and revision", &debug.BuildInfo{
			Main:     debug.Module{Version: "v0.3.0"},
			Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "0123456789abcdef0123"}},
		}, "v0.3.0+0123456789ab"},
		{"revision only", &debug.BuildInfo{
			Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc"}},
		}, Unknown + "+abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := fromBuildInfo(tc.info); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStringNeverEmpty(t *testing.T) {
	t.Parallel()
	if String() == "" {
		t.Fatal("String() returned empty")
	}
}
