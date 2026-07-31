package clankcli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acksell/clank/internal/host/preview"
)

// packagerFixture creates a project dir with the given files plus a
// .git dir so ResolvePackager's walk-up stays inside the fixture.
func packagerFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The one-time packager question: fires once for a non-bun project,
// persists either answer, and never asks again. Not parallel:
// CLANK_DIR pins where choices are saved.
func TestPromptPackagerChoice(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir())

	t.Run("yes switches to bun and persists", func(t *testing.T) {
		dir := packagerFixture(t, map[string]string{
			"package.json":      `{"devDependencies":{"vite":"^6"}}`,
			"package-lock.json": "{}",
		})
		var out strings.Builder
		promptPackagerChoice(dir, bytes.NewBufferString("y\n"), &out, true)

		if !strings.Contains(out.String(), "Detected npm (package-lock.json)") {
			t.Errorf("missing detection line: %q", out.String())
		}
		if !strings.Contains(out.String(), "Clank prefers bun") {
			t.Errorf("missing recommendation: %q", out.String())
		}
		pm, ok := preview.LoadPackagerChoice(dir)
		if !ok || pm != preview.PackagerBun {
			t.Errorf("saved choice = %q, %v; want bun, true", pm, ok)
		}
	})

	t.Run("no keeps the detected manager and persists", func(t *testing.T) {
		dir := packagerFixture(t, map[string]string{
			"package.json":   `{"devDependencies":{"vite":"^6"}}`,
			"pnpm-lock.yaml": "",
		})
		var out strings.Builder
		promptPackagerChoice(dir, bytes.NewBufferString("\n"), &out, true)

		pm, ok := preview.LoadPackagerChoice(dir)
		if !ok || pm != preview.PackagerPNPM {
			t.Errorf("saved choice = %q, %v; want pnpm, true", pm, ok)
		}

		// Second run: saved choice short-circuits, no re-prompt (an
		// empty stdin would fail readYes if it were consulted).
		var again strings.Builder
		promptPackagerChoice(dir, bytes.NewBuffer(nil), &again, true)
		if !strings.Contains(again.String(), "your saved choice") {
			t.Errorf("second run did not honor the saved choice: %q", again.String())
		}
		if strings.Contains(again.String(), "[y/N]") {
			t.Errorf("second run re-prompted: %q", again.String())
		}
	})

	t.Run("non-interactive narrates but saves nothing", func(t *testing.T) {
		dir := packagerFixture(t, map[string]string{
			"package.json": `{"devDependencies":{"vite":"^6"}}`,
			"yarn.lock":    "",
		})
		var out strings.Builder
		promptPackagerChoice(dir, bytes.NewBuffer(nil), &out, false)

		if !strings.Contains(out.String(), "Installing with yarn.") {
			t.Errorf("missing narration: %q", out.String())
		}
		if strings.Contains(out.String(), "[y/N]") {
			t.Errorf("non-interactive run prompted: %q", out.String())
		}
		if _, ok := preview.LoadPackagerChoice(dir); ok {
			t.Error("non-interactive run saved a choice — a later interactive run can never ask")
		}
	})

	t.Run("bun via repo signal narrates without a question", func(t *testing.T) {
		dir := packagerFixture(t, map[string]string{
			"package.json": `{"devDependencies":{"vite":"^6"}}`,
			"bun.lock":     "",
		})
		var out strings.Builder
		promptPackagerChoice(dir, bytes.NewBuffer(nil), &out, true)
		if !strings.Contains(out.String(), "Installing with bun (bun.lock)") {
			t.Errorf("missing bun narration: %q", out.String())
		}
		if strings.Contains(out.String(), "[y/N]") {
			t.Errorf("bun project prompted: %q", out.String())
		}
	})

	t.Run("no signal stays silent", func(t *testing.T) {
		dir := packagerFixture(t, map[string]string{
			"package.json": `{"devDependencies":{"vite":"^6"}}`,
		})
		var out strings.Builder
		promptPackagerChoice(dir, bytes.NewBuffer(nil), &out, true)
		if out.Len() != 0 {
			t.Errorf("template-style project produced output: %q", out.String())
		}
	})
}
