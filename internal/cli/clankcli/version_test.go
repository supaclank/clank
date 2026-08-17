package clankcli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/supaclank/clank/internal/version"
)

func TestVersionCommandPrintsVersion(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"version"}, {"--version"}} {
		root := Command()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if got := out.String(); !strings.Contains(got, version.String()) {
			t.Fatalf("%v: output %q does not contain %q", args, got, version.String())
		}
	}
}
