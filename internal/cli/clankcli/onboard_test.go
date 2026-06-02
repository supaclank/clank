package clankcli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/acksell/clank/internal/clanksync/triggers"
)

func newPromptCmd(in string) (*cobra.Command, *bytes.Buffer) {
	cmd := &cobra.Command{}
	out := &bytes.Buffer{}
	cmd.SetIn(strings.NewReader(in))
	cmd.SetOut(out)
	cmd.SetErr(out)
	return cmd, out
}

func TestConfirmYesNo_Default(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in         string
		defaultYes bool
		want       bool
	}{
		{"\n", true, true},   // bare Enter honors default-yes
		{"\n", false, false}, // bare Enter honors default-no
		{"y\n", false, true},
		{"yes\n", false, true},
		{"n\n", true, false},
		{"no\n", true, false},
		{"garbage\n", true, false}, // unrecognized → not yes
	}
	for _, tc := range cases {
		cmd, _ := newPromptCmd(tc.in)
		got, err := confirmYesNo(cmd, "? ", tc.defaultYes)
		if err != nil {
			t.Fatalf("confirmYesNo(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("confirmYesNo(in=%q, default=%v) = %v, want %v", tc.in, tc.defaultYes, got, tc.want)
		}
	}
}

// TestReadLine_NoOverReadAcrossPrompts pins that two sequential prompts
// sharing one reader each get their own line — readLine must not buffer
// past the newline (a bufio.Reader would swallow the second answer).
func TestReadLine_NoOverReadAcrossPrompts(t *testing.T) {
	t.Parallel()
	cmd, _ := newPromptCmd("y\nn\n")
	first, err := confirmYesNo(cmd, "first? ", false)
	if err != nil || !first {
		t.Fatalf("first prompt: got=%v err=%v, want true", first, err)
	}
	second, err := confirmYesNo(cmd, "second? ", true)
	if err != nil {
		t.Fatal(err)
	}
	if second {
		t.Fatal("second prompt lost input: got default(true), want false from the 'n' line")
	}
}

func TestIsInteractive_FalseForBuffers(t *testing.T) {
	t.Parallel()
	cmd, _ := newPromptCmd("")
	if isInteractive(cmd) {
		t.Error("isInteractive must be false when in/out are not terminals (autopush-hook safety)")
	}
}

// TestInstallTriggersFor_InstallsOnlyChosen pins per-harness selection:
// installing one harness must not create the other's trigger file.
func TestInstallTriggersFor_InstallsOnlyChosen(t *testing.T) {
	// t.Setenv forbids t.Parallel.
	tmp := t.TempDir()
	claudeDir := filepath.Join(tmp, "claude")
	xdgDir := filepath.Join(tmp, "xdg")
	t.Setenv("HOME", tmp)
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	t.Setenv("XDG_CONFIG_HOME", xdgDir)

	claudeSettings := filepath.Join(claudeDir, "settings.json")
	opencodePlugin := filepath.Join(xdgDir, "opencode", "plugins", "clank-autopush.ts")

	cmd, _ := newPromptCmd("")

	// Claude only.
	if err := installTriggersFor(cmd, []string{triggers.HarnessClaudeCode}); err != nil {
		t.Fatalf("install claude: %v", err)
	}
	if !fileExists(claudeSettings) {
		t.Error("claude settings.json not written")
	}
	if fileExists(opencodePlugin) {
		t.Error("opencode plugin written despite claude-only selection")
	}

	// opencode only, in a fresh dir set.
	tmp2 := t.TempDir()
	t.Setenv("HOME", tmp2)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(tmp2, "claude"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp2, "xdg"))
	if err := installTriggersFor(cmd, []string{triggers.HarnessOpenCode}); err != nil {
		t.Fatalf("install opencode: %v", err)
	}
	if !fileExists(filepath.Join(tmp2, "xdg", "opencode", "plugins", "clank-autopush.ts")) {
		t.Error("opencode plugin not written")
	}
	if fileExists(filepath.Join(tmp2, "claude", "settings.json")) {
		t.Error("claude settings written despite opencode-only selection")
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
