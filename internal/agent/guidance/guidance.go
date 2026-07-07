// Package guidance assembles the stack-specific guidance that clank injects as
// the building agent's system prompt at session start, and materializes the
// stack's detailed playbook as an on-demand skill in the project tree. The
// stack is detected by inspecting the project's package.json; today only
// Expo / React Native is recognized, returning "" for anything else.
//
// The guidance is deliberately two-layer. The system prompt carries only the
// distilled reasoning principles (~half a KB of tokens): a large system prompt
// costs every request and measurably pushes models toward longer thinking, so
// the detailed mechanisms, case studies, and checklists live in a skill
// (.claude/skills/<name>/) that the agent reads on demand. The prompt tells
// the agent the skill exists; the skill's SKILL.md indexes the references.
//
// Scope is deliberately narrow: only static, stack-specific guidance lives here.
// Cross-stack framing and session-environment context (how the session was
// created, which tools are enabled, the preview environment) belong to a future
// dynamic layer that composes with this output — that context is runtime-derived
// and is not baked into these packs.
package guidance

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed docs
var docsFS embed.FS

// Stack identifies the detected project tech stack.
type Stack string

const (
	StackUnknown Stack = ""
	StackExpo    Stack = "expo"
)

// expoDependency is the package whose presence in package.json marks an Expo
// project. Every Expo app depends on it, and the Expo CLI itself keys off this —
// so it is both necessary and sufficient as the detection signal.
const expoDependency = "expo"

// expoPack lists the embedded docs concatenated, in order, to form the Expo
// system prompt. Kept to the distilled principles doc on purpose — the
// detailed playbook ships as the skill below, not in the prompt.
var expoPack = []string{
	"docs/expo/prompt.md",
}

// expoSkillRelDir is where InstallSkills materializes the Expo playbook,
// relative to the project root. The path is what Claude Code scans for
// project-level skills, and prompt.md points the agent at it by name.
const expoSkillRelDir = ".claude/skills/expo-dev"

// expoSkillFiles are the embedded sources written (flattened, by base name)
// into expoSkillRelDir. SKILL.md must lead: it carries the skill frontmatter
// and indexes the rest.
var expoSkillFiles = []string{
	"docs/expo/skill/SKILL.md",
	"docs/expo/skill/dependencies.md",
	"docs/expo/skill/performance.md",
	"docs/expo/skill/ux.md",
}

// DetectStack inspects workDir/package.json and returns the project's stack.
// A missing, unreadable, or non-Expo package.json yields StackUnknown — the
// absence of the signal is the answer, not an error.
func DetectStack(workDir string) Stack {
	if workDir == "" {
		return StackUnknown
	}
	data, err := os.ReadFile(filepath.Join(workDir, "package.json"))
	if err != nil {
		return StackUnknown
	}
	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return StackUnknown
	}
	if _, ok := pkg.Dependencies[expoDependency]; ok {
		return StackExpo
	}
	if _, ok := pkg.DevDependencies[expoDependency]; ok {
		return StackExpo
	}
	return StackUnknown
}

// Assemble returns the guidance to inject as the agent's system prompt for the
// project at workDir, or "" when no pack applies. The result is stable for a
// given stack, so it caches cleanly when sent as a system prompt.
func Assemble(workDir string) string {
	switch DetectStack(workDir) {
	case StackExpo:
		return readPack(expoPack)
	default:
		return ""
	}
}

// installLocks serializes concurrent InstallSkills calls for the same
// workDir — two backends starting in the same project at once must not
// interleave writes to the shared skill files or info/exclude.
var installLocks sync.Map // map[string]*sync.Mutex, keyed by workDir

func lockInstall(workDir string) func() {
	v, _ := installLocks.LoadOrStore(workDir, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// InstallSkills materializes the detected stack's playbook into the project's
// .claude/skills directory so the agent can read it on demand, and excludes
// the path from git so an agent's `git add -A` never stages clank-managed
// files into the user's repo. Idempotent — files are overwritten on every
// call, so a clank upgrade refreshes stale copies in existing worktrees.
// No-op for unknown stacks.
func InstallSkills(workDir string) error {
	switch DetectStack(workDir) {
	case StackExpo:
		unlock := lockInstall(workDir)
		defer unlock()
		return installSkillFiles(workDir, expoSkillRelDir, expoSkillFiles)
	default:
		return nil
	}
}

// installSkillFiles writes the embedded paths (flattened to their base names)
// under workDir/relDir and registers relDir in the repo's git exclude file.
func installSkillFiles(workDir, relDir string, srcPaths []string) error {
	dst := filepath.Join(workDir, filepath.FromSlash(relDir))
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("guidance: create skill dir: %w", err)
	}
	for _, p := range srcPaths {
		data, err := docsFS.ReadFile(p)
		if err != nil {
			// A typo'd embed path is a programmer error — surface it so the
			// pack tests catch it, mirroring readPack's marker-check guard.
			return fmt.Errorf("guidance: embedded skill doc %s: %w", p, err)
		}
		if err := os.WriteFile(filepath.Join(dst, path.Base(p)), data, 0o644); err != nil {
			return fmt.Errorf("guidance: write skill doc: %w", err)
		}
	}
	return ensureGitExcluded(workDir, relDir)
}

// gitSubprocessTimeout bounds the rev-parse calls in ensureGitExcluded so a
// hung git (network mount, lock contention, a misbehaving hook) can't leak
// the process or the goroutine InstallSkills now runs in.
const gitSubprocessTimeout = 5 * time.Second

// ensureGitExcluded appends relDir to the repository's info/exclude so the
// materialized skill never shows up in git status or gets staged by an
// agent's `git add -A`. The exclude file is resolved with
// `git rev-parse --git-path info/exclude`, which is correct for linked
// worktrees (clank's normal layout) as well as plain checkouts. Best-effort
// by design: no git binary, not a repo, or an unwritable exclude file leaves
// the skill functional — it would merely be visible to git.
func ensureGitExcluded(workDir, relDir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), gitSubprocessTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", workDir, "rev-parse", "--git-path", "info/exclude").Output()
	if err != nil {
		return nil
	}
	excludePath := strings.TrimSpace(string(out))
	if excludePath == "" {
		return nil
	}
	if !filepath.IsAbs(excludePath) {
		excludePath = filepath.Join(workDir, excludePath)
	}
	// info/exclude patterns are anchored to the repo root, not workDir, so a
	// workDir nested below the root (monorepo layouts) needs the repo-relative
	// prefix or the pattern silently fails to match.
	var repoPrefix string
	if out, err := exec.CommandContext(ctx, "git", "-C", workDir, "rev-parse", "--show-prefix").Output(); err == nil {
		repoPrefix = strings.TrimSpace(string(out))
	}
	line := "/" + repoPrefix + relDir + "/"
	existing, _ := os.ReadFile(excludePath)
	for _, l := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(l) == line {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		log.Printf("guidance: create exclude dir %s: %v", filepath.Dir(excludePath), err)
		return nil
	}
	f, err := os.OpenFile(excludePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("guidance: open exclude file %s: %v", excludePath, err)
		return nil
	}
	defer f.Close()
	prefix := ""
	if n := len(existing); n > 0 && existing[n-1] != '\n' {
		prefix = "\n"
	}
	if _, err := f.WriteString(prefix + line + "\n"); err != nil {
		log.Printf("guidance: write exclude entry to %s: %v", excludePath, err)
	}
	return nil
}

// readPack concatenates embedded docs with blank-line separators. Skipping on
// read error is safe: a typo'd path in the pack fails TestAssembleExpo's marker
// checks, not silently ships.
func readPack(paths []string) string {
	var b strings.Builder
	for _, p := range paths {
		data, err := docsFS.ReadFile(p)
		if err != nil {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.Write(data)
	}
	return b.String()
}
