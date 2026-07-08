package guidance_test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/acksell/clank/internal/agent/guidance"
)

func writePackageJSON(t *testing.T, dir, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(contents), 0o644); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
}

func TestDetectStack(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		pkgJSON string // "" means no package.json file at all
		want    guidance.Stack
	}{
		{"expo in dependencies", `{"dependencies":{"expo":"~51.0.0","react-native":"0.74.0"}}`, guidance.StackExpo},
		{"expo in devDependencies", `{"devDependencies":{"expo":"~51.0.0"}}`, guidance.StackExpo},
		{"react-native without expo", `{"dependencies":{"react-native":"0.74.0"}}`, guidance.StackUnknown},
		{"plain node project", `{"dependencies":{"express":"4.0.0"}}`, guidance.StackUnknown},
		{"malformed package.json", `{not valid json`, guidance.StackUnknown},
		{"no package.json", "", guidance.StackUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if tt.pkgJSON != "" {
				writePackageJSON(t, dir, tt.pkgJSON)
			}
			if got := guidance.DetectStack(dir); got != tt.want {
				t.Errorf("DetectStack = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAssembleExpo(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePackageJSON(t, dir, `{"dependencies":{"expo":"~51.0.0"}}`)

	got := guidance.Assemble(dir)
	if got == "" {
		t.Fatal("Assemble returned empty for an Expo project")
	}
	// Markers prove the distilled prompt was read (guards readPack against a
	// typo'd embed path) and that it still carries the two load-bearing parts:
	// the install rule and the pointer at the on-demand skill.
	for _, marker := range []string{
		"existing Expo",    // framing
		"npx expo install", // the #1 environment-breaker rule
		"expo-dev",         // the skill pointer
	} {
		if !strings.Contains(got, marker) {
			t.Errorf("Assemble output missing marker %q", marker)
		}
	}
	// The detailed playbook must NOT be in the prompt — it was ~7.5k tokens of
	// system prompt before it moved into the skill. Case-study markers from
	// performance.md leaking back in means the pack regressed to front-loading.
	for _, heavy := range []string{"gfxinfo", "SectionList", "Skia"} {
		if strings.Contains(got, heavy) {
			t.Errorf("Assemble output contains %q — detailed playbook content belongs in the skill, not the prompt", heavy)
		}
	}
	// Budget guard: the whole point of the split is a small prompt. 4KB is
	// roughly 1k tokens — comfortably above the current ~2KB doc, far below
	// the 30KB it replaced.
	if len(got) > 4096 {
		t.Errorf("Assemble output is %d bytes; the distilled prompt must stay under 4096", len(got))
	}
}

func TestAssembleNonExpo(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePackageJSON(t, dir, `{"dependencies":{"react":"18.0.0"}}`)

	if got := guidance.Assemble(dir); got != "" {
		t.Errorf("Assemble = %q, want empty for non-Expo project", got)
	}
}

func TestInstallSkillsExpo(t *testing.T) {
	// No t.Parallel(): t.Setenv is incompatible with parallel tests.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := t.TempDir()
	writePackageJSON(t, dir, `{"dependencies":{"expo":"~51.0.0"}}`)

	if err := guidance.InstallSkills(dir); err != nil {
		t.Fatalf("InstallSkills: %v", err)
	}
	skillDir := filepath.Join(home, ".claude", "skills", "expo-dev")
	for _, f := range []string{"SKILL.md", "dependencies.md", "performance.md", "ux.md"} {
		data, err := os.ReadFile(filepath.Join(skillDir, f))
		if err != nil {
			t.Fatalf("skill file %s: %v", f, err)
		}
		if len(data) == 0 {
			t.Errorf("skill file %s is empty", f)
		}
	}
	// SKILL.md must open with frontmatter naming the skill — that's what the
	// distilled prompt references and what Claude Code's skill discovery reads.
	skill, err := os.ReadFile(filepath.Join(skillDir, "SKILL.md"))
	if err != nil {
		t.Fatalf("read SKILL.md: %v", err)
	}
	normalized := strings.ReplaceAll(string(skill), "\r\n", "\n")
	if !strings.HasPrefix(normalized, "---\nname: expo-dev\n") {
		preview := normalized
		if len(preview) > 40 {
			preview = preview[:40]
		}
		t.Errorf("SKILL.md must start with frontmatter naming expo-dev; got prefix %q", preview)
	}

	// Idempotent: a second install must succeed and leave the files in place.
	if err := guidance.InstallSkills(dir); err != nil {
		t.Fatalf("second InstallSkills: %v", err)
	}
}

func TestInstallSkillsConcurrent(t *testing.T) {
	// No t.Parallel(): t.Setenv is incompatible with parallel tests.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := t.TempDir()
	writePackageJSON(t, dir, `{"dependencies":{"expo":"~51.0.0"}}`)

	skillDir := filepath.Join(home, ".claude", "skills", "expo-dev")
	files := []string{"SKILL.md", "dependencies.md", "performance.md", "ux.md"}

	// Baseline: a single install gives the expected byte-for-byte content.
	if err := guidance.InstallSkills(dir); err != nil {
		t.Fatalf("baseline InstallSkills: %v", err)
	}
	want := make(map[string][]byte, len(files))
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(skillDir, f))
		if err != nil {
			t.Fatalf("read baseline %s: %v", f, err)
		}
		want[f] = data
	}

	// installGuidanceSkills (internal/host) fires one goroutine per session
	// create/resume, all targeting this same shared directory — reproduce
	// that concurrency here rather than mocking it.
	const installers = 20
	errs := make(chan error, installers)
	var wg sync.WaitGroup
	for i := 0; i < installers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- guidance.InstallSkills(dir)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent InstallSkills: %v", err)
		}
	}

	// Every file must still match byte-for-byte — a torn or truncated write
	// from one goroutine's open(O_TRUNC) landing between another's open and
	// write would corrupt or shorten the content.
	for _, f := range files {
		got, err := os.ReadFile(filepath.Join(skillDir, f))
		if err != nil {
			t.Fatalf("skill file %s: %v", f, err)
		}
		if string(got) != string(want[f]) {
			t.Errorf("skill file %s corrupted by concurrent installs: got %d bytes, want %d", f, len(got), len(want[f]))
		}
	}
}

func TestInstallSkillsNonExpo(t *testing.T) {
	// No t.Parallel(): t.Setenv is incompatible with parallel tests.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := t.TempDir()
	writePackageJSON(t, dir, `{"dependencies":{"express":"4.0.0"}}`)

	if err := guidance.InstallSkills(dir); err != nil {
		t.Fatalf("InstallSkills: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); !os.IsNotExist(err) {
		t.Errorf("~/.claude must not be created for a non-Expo project (stat err: %v)", err)
	}
	// The project tree stays untouched either way — skills live in HOME.
	if _, err := os.Stat(filepath.Join(dir, ".claude")); !os.IsNotExist(err) {
		t.Errorf("project .claude must never be created by InstallSkills (stat err: %v)", err)
	}
}
