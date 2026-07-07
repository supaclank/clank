package guidance_test

import (
	"os"
	"os/exec"
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
	t.Parallel()
	dir := t.TempDir()
	writePackageJSON(t, dir, `{"dependencies":{"expo":"~51.0.0"}}`)

	if err := guidance.InstallSkills(dir); err != nil {
		t.Fatalf("InstallSkills: %v", err)
	}
	skillDir := filepath.Join(dir, ".claude", "skills", "expo-dev")
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
	if !strings.HasPrefix(string(skill), "---\nname: expo-dev\n") {
		n := min(len(skill), 40)
		t.Errorf("SKILL.md must start with frontmatter naming expo-dev; got prefix %q", string(skill[:n]))
	}

	// Idempotent: a second install must succeed and leave the files in place.
	if err := guidance.InstallSkills(dir); err != nil {
		t.Fatalf("second InstallSkills: %v", err)
	}
}

func TestInstallSkillsNonExpo(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePackageJSON(t, dir, `{"dependencies":{"express":"4.0.0"}}`)

	if err := guidance.InstallSkills(dir); err != nil {
		t.Fatalf("InstallSkills: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude")); !os.IsNotExist(err) {
		t.Errorf(".claude must not be created for a non-Expo project (stat err: %v)", err)
	}
}

// TestInstallSkillsGitExclude: in a git repo, the materialized skill must be
// invisible to git — an agent's `git add -A` staging clank-managed files into
// the user's repo is exactly what the exclude prevents.
func TestInstallSkillsGitExclude(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	writePackageJSON(t, dir, `{"dependencies":{"expo":"~51.0.0"}}`)

	if err := guidance.InstallSkills(dir); err != nil {
		t.Fatalf("InstallSkills: %v", err)
	}
	out, err := exec.Command("git", "-C", dir, "status", "--porcelain").Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if strings.Contains(string(out), ".claude") {
		t.Errorf("git status shows the skill dir — exclude did not take:\n%s", out)
	}
	// Idempotent on the exclude file too: same content after a re-install.
	excludePath := filepath.Join(dir, ".git", "info", "exclude")
	before, _ := os.ReadFile(excludePath)
	if err := guidance.InstallSkills(dir); err != nil {
		t.Fatalf("second InstallSkills: %v", err)
	}
	after, _ := os.ReadFile(excludePath)
	if string(before) != string(after) {
		t.Errorf("exclude file grew on re-install:\nbefore: %q\nafter: %q", before, after)
	}
}

// TestInstallSkillsGitExcludeMonorepo: info/exclude patterns are anchored to
// the repo root, so a workDir nested below it (monorepo layouts) needs the
// repo-relative prefix or the exclude entry silently fails to match.
func TestInstallSkillsGitExcludeMonorepo(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
	} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	appDir := filepath.Join(root, "apps", "mobile")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatalf("mkdir app dir: %v", err)
	}
	writePackageJSON(t, appDir, `{"dependencies":{"expo":"~51.0.0"}}`)
	// Commit the baseline first: otherwise the untracked package.json would
	// make "apps/" show up as untracked regardless of whether .claude is
	// excluded, masking the bug this test exists to catch.
	for _, args := range [][]string{
		{"add", "-A"},
		{"commit", "-q", "-m", "baseline"},
	} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	if err := guidance.InstallSkills(appDir); err != nil {
		t.Fatalf("InstallSkills: %v", err)
	}
	out, err := exec.Command("git", "-C", root, "status", "--porcelain").Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if strings.Contains(string(out), ".claude") {
		t.Errorf("git status shows the skill dir from a nested workDir — exclude did not take:\n%s", out)
	}
}

// TestInstallSkillsConcurrent: backends.go runs InstallSkills in a goroutine
// on every CreateBackend (fresh and resumed), so two sessions touching the
// same workDir around the same time can call it concurrently. The
// check-then-append in ensureGitExcluded is not atomic on its own — installMu
// is what makes a concurrent burst converge on exactly one exclude entry
// instead of racing to duplicate it.
func TestInstallSkillsConcurrent(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	writePackageJSON(t, dir, `{"dependencies":{"expo":"~51.0.0"}}`)

	const n = 50
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = guidance.InstallSkills(dir)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("InstallSkills[%d]: %v", i, err)
		}
	}

	excludePath := filepath.Join(dir, ".git", "info", "exclude")
	data, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read exclude file: %v", err)
	}
	count := 0
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) == "/.claude/skills/expo-dev/" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 exclude entry after %d concurrent installs, got %d:\n%s", n, count, data)
	}
}
