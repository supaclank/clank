package clankcli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestShellQuoteJoin(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"plain args pass through", []string{"npx", "expo", "run:android", "--device"}, "npx expo run:android --device"},
		{"arg with spaces requoted", []string{"grep", "a b", "file"}, "grep 'a b' file"},
		{"embedded single quote", []string{"echo", "don't"}, `echo 'don'\''t'`},
		{"shell metachars quoted", []string{"sh", "-c", "make 2>&1 | tail"}, `sh -c 'make 2>&1 | tail'`},
		{"empty arg preserved", []string{"cmd", ""}, "cmd ''"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := shellQuoteJoin(tc.argv); got != tc.want {
				t.Errorf("shellQuoteJoin(%q):\n got %s\nwant %s", tc.argv, got, tc.want)
			}
		})
	}
}

// TestFixCmd_ChildFlagsStayInArgs pins the SetInterspersed(false)
// wiring: flags after the first positional belong to the command under
// debug, not to cobra. Asserted through the args cobra hands RunE by
// swapping RunE for a recorder — no daemon involved.
func TestFixCmd_ChildFlagsStayInArgs(t *testing.T) {
	t.Parallel()

	cmd := fixCmd()
	var got []string
	cmd.RunE = func(_ *cobra.Command, args []string) error {
		got = append([]string{}, args...)
		return nil
	}
	cmd.SetArgs([]string{"npx", "expo", "run:android", "--device"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := []string{"npx", "expo", "run:android", "--device"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("args: got %q, want %q", got, want)
	}
}

// TestFixCmd_OwnFlagsBeforeCommandStillParse: clank-owned flags work
// when given before the command (the only supported position with
// interspersed parsing off).
func TestFixCmd_OwnFlagsBeforeCommandStillParse(t *testing.T) {
	t.Parallel()

	cmd := fixCmd()
	var got []string
	cmd.RunE = func(c *cobra.Command, args []string) error {
		got = append([]string{}, args...)
		return nil
	}
	cmd.SetArgs([]string{"--project", "/tmp/x", "make", "-j4"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Join(got, " ") != "make -j4" {
		t.Errorf("args: got %q, want [make -j4]", got)
	}
	if v, _ := cmd.Flags().GetString("project"); v != "/tmp/x" {
		t.Errorf("--project: got %q, want /tmp/x", v)
	}
}

// TestFixPrompt_ContainsCommandLine pins the template instantiation the
// integration path sends: the quoted command line lands inside the
// backticks and the monitoring instructions survive.
func TestFixPrompt_ContainsCommandLine(t *testing.T) {
	t.Parallel()

	prompt := fixPrompt([]string{"npx", "expo", "run:android"})
	if !strings.Contains(prompt, "<command>npx expo run:android</command>") {
		t.Errorf("prompt %q lacks the quoted command line", prompt)
	}
	if !strings.Contains(prompt, "background") || !strings.Contains(prompt, "diagnose") {
		t.Errorf("prompt %q lost the monitoring/debugging instructions", prompt)
	}
}
