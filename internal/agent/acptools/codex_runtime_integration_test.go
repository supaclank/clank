package acptools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestIntegration_CodexRuntimeSharedWithAdapter(t *testing.T) {
	if os.Getenv("CLANK_TEST_CODEX_ACP") == "" {
		t.Skip("set CLANK_TEST_CODEX_ACP=1 to verify the installed Codex runtime")
	}
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), installTimeout)
	defer cancel()
	paths, err := Ensure(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Resolve from the adapter so a nested, older Codex cannot escape the pin.
	cmd := exec.CommandContext(ctx, paths.BunBin, "-e", `console.log(require.resolve("@openai/codex/bin/codex.js", {paths: [process.argv[1]]}))`, filepath.Dir(paths.CodexACPEntry))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("resolve adapter Codex runtime: %v: %s", err, out)
	}
	adapterBin := strings.TrimSpace(string(out))
	adapterInfo, err := os.Stat(adapterBin)
	if err != nil {
		t.Fatal(err)
	}
	loginInfo, err := os.Stat(paths.CodexBin)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(adapterInfo, loginInfo) {
		t.Fatalf("adapter uses %s, login uses %s; both must use the pinned Codex", adapterBin, paths.CodexBin)
	}
	cmd = exec.CommandContext(ctx, paths.BunBin, adapterBin, "--version")
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Codex version: %v: %s", err, out)
	}
	if got, want := strings.TrimSpace(string(out)), "codex-cli "+PinnedCodexVersion; got != want {
		t.Fatalf("Codex version = %q, want %q", got, want)
	}
}
