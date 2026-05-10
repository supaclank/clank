package checkpoint

import (
	"reflect"
	"sort"
	"testing"
)

// TestScrubGitEnv pins the contract: every GIT_* var that overrides
// repository discovery (GIT_DIR, GIT_WORK_TREE, GIT_INDEX_FILE, etc.)
// is stripped, and unrelated env vars survive untouched.
func TestScrubGitEnv(t *testing.T) {
	t.Parallel()
	in := []string{
		"PATH=/usr/bin",
		"GIT_DIR=/should/be/dropped",
		"HOME=/home/me",
		"GIT_WORK_TREE=/also/dropped",
		"GIT_OBJECT_DIRECTORY=/dropped",
		"GIT_COMMON_DIR=/dropped",
		"GIT_NAMESPACE=dropped",
		"GIT_INDEX_FILE=/dropped",
		"GIT_CONFIG=/dropped",
		"GIT_CONFIG_GLOBAL=/dropped",
		"GIT_CONFIG_SYSTEM=/dropped",
		"GIT_AUTHOR_NAME=keep-me",
		"NO_EQUALS_SIGN",
	}
	want := []string{
		"PATH=/usr/bin",
		"HOME=/home/me",
		"GIT_AUTHOR_NAME=keep-me",
		"NO_EQUALS_SIGN",
	}
	got := scrubGitEnv(in)
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scrubGitEnv mismatch\nwant: %q\ngot:  %q", want, got)
	}
}
