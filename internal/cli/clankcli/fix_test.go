package clankcli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host/hosttest"
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
		{"history expansion chars quoted", []string{"echo", "hello!", "a^b"}, "echo 'hello!' 'a^b'"},
		{"carriage return quoted", []string{"echo", "hello\rworld"}, "echo 'hello\rworld'"},
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

// TestRunFix_CreatesSessionWithFixPrompt exercises runFix against a real
// host + stub backend (see newTestHost in please_test.go), pinning the
// call into runPrompt: a stacked-branch rebase once desynced this call
// site from runPrompt's signature without a test catching it (build
// failure only), since fix_test.go only drove fixCmd's flag parsing.
func TestRunFix_CreatesSessionWithFixPrompt(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir())

	client, stub := newTestHost(t)
	repo := hosttest.InitGitRepo(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var out, errOut bytes.Buffer
	err := runFix(ctx, client, &out, &errOut, promptOpts{
		backend:    agent.BackendOpenCode,
		projectDir: repo,
	}, []string{"npx", "expo", "run:android"})
	if err != nil {
		t.Fatalf("runFix: %v", err)
	}
	if errOut.Len() != 0 {
		t.Errorf("errOut: got %q, want empty", errOut.String())
	}

	last := stub.Last()
	if last == nil {
		t.Fatal("no backend created — session was not started")
	}
	if got := last.LastSendOpts().Text; !strings.Contains(got, "<command>npx expo run:android</command>") {
		t.Errorf("prompt sent to backend %q lacks the quoted command", got)
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
