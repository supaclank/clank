package clankcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The packager note is narration only: no questions, no stored state,
// and it reads the same Resolution the daemon acts on.
func TestPrintPackagerNote(t *testing.T) {
	t.Parallel()

	t.Run("non-bun project gets the plan and the tip", func(t *testing.T) {
		t.Parallel()
		dir := packagerFixture(t, map[string]string{
			"package.json":   `{"devDependencies":{"vite":"^6"}}`,
			"pnpm-lock.yaml": "",
		})
		var out strings.Builder
		printPackagerNote(dir, &out)
		if !strings.Contains(out.String(), "Installing with pnpm (pnpm-lock.yaml)") {
			t.Errorf("missing install plan: %q", out.String())
		}
		if !strings.Contains(out.String(), "bun install") {
			t.Errorf("missing the bun tip: %q", out.String())
		}
		if strings.Contains(out.String(), "[y/N]") {
			t.Errorf("the note must never ask anything: %q", out.String())
		}
	})

	t.Run("existing dependencies are called out as untouched", func(t *testing.T) {
		t.Parallel()
		dir := packagerFixture(t, map[string]string{
			"package.json":               `{"devDependencies":{"vite":"^6"}}`,
			"package-lock.json":          "{}",
			"node_modules/left-pad/x.js": "x",
		})
		var out strings.Builder
		printPackagerNote(dir, &out)
		if !strings.Contains(out.String(), "existing dependencies") {
			t.Errorf("missing the untouched-tree narration: %q", out.String())
		}
		if strings.Contains(out.String(), "Installing with npm") {
			t.Errorf("claims an install that will be skipped: %q", out.String())
		}
	})

	t.Run("clank.yaml pinned install narrates the config, no tip machinery", func(t *testing.T) {
		t.Parallel()
		dir := packagerFixture(t, map[string]string{
			"package.json":   `{"devDependencies":{"vite":"^6"}}`,
			"pnpm-lock.yaml": "",
			"clank.yaml":     "preview:\n  install: pnpm install --frozen-lockfile\n",
		})
		var out strings.Builder
		printPackagerNote(dir, &out)
		if !strings.Contains(out.String(), "clank.yaml") {
			t.Errorf("missing config narration: %q", out.String())
		}
		if strings.Contains(out.String(), "Tip:") {
			t.Errorf("pinned installs need no tip: %q", out.String())
		}
	})

	t.Run("clank.yaml dir re-roots the deps-present check", func(t *testing.T) {
		t.Parallel()
		dir := packagerFixture(t, map[string]string{
			"clank.yaml":                         "preview:\n  dir: web-app\n",
			"web-app/package.json":               `{"devDependencies":{"vite":"^6"}}`,
			"web-app/package-lock.json":          "{}",
			"web-app/node_modules/left-pad/x.js": "x",
		})
		var out strings.Builder
		printPackagerNote(dir, &out)
		if !strings.Contains(out.String(), "existing dependencies") {
			t.Errorf("deps-present check did not follow preview.dir: %q", out.String())
		}
	})

	t.Run("bun project narrates its evidence, no tip", func(t *testing.T) {
		t.Parallel()
		dir := packagerFixture(t, map[string]string{
			"package.json": `{"devDependencies":{"vite":"^6"}}`,
			"bun.lock":     "",
		})
		var out strings.Builder
		printPackagerNote(dir, &out)
		if !strings.Contains(out.String(), "Installing with bun (bun.lock)") {
			t.Errorf("missing bun narration: %q", out.String())
		}
		if strings.Contains(out.String(), "Tip:") {
			t.Errorf("bun projects need no tip: %q", out.String())
		}
	})

	t.Run("missing dir stays silent, never prompts off the parent's signals", func(t *testing.T) {
		dir := packagerFixture(t, map[string]string{
			"package.json":      `{"devDependencies":{"vite":"^6"}}`,
			"package-lock.json": "{}",
			"clank.yaml":        "preview:\n  dir: gone\n",
		})
		var out strings.Builder
		promptPackagerChoice(dir, bytes.NewBufferString("y\n"), &out, true)
		if out.Len() != 0 {
			t.Errorf("missing preview.dir produced output: %q (Start will reject this config anyway)", out.String())
		}
		if _, ok := preview.LoadPackagerChoice(filepath.Join(dir, "gone")); ok {
			t.Error("missing preview.dir saved a choice keyed to a directory that doesn't exist")
		}
	})

	t.Run("no signal stays silent", func(t *testing.T) {
		t.Parallel()
		dir := packagerFixture(t, map[string]string{
			"package.json": `{"devDependencies":{"vite":"^6"}}`,
		})
		var out strings.Builder
		printPackagerNote(dir, &out)
		if out.Len() != 0 {
			t.Errorf("template-style project produced output: %q", out.String())
		}
	})
}
