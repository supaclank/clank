// Package guidance assembles the stack-specific guidance that clank injects as
// the building agent's system prompt at session start, and materializes the
// stack's detailed playbook as an on-demand skill in the user's personal
// skills directory (~/.claude/skills). The stack is detected by inspecting
// the project's package.json; today only Expo / React Native is recognized,
// returning "" for anything else.
//
// The guidance is deliberately two-layer. The system prompt carries only the
// distilled reasoning principles (~half a KB of tokens): a large system prompt
// costs every request and measurably pushes models toward longer thinking, so
// the detailed mechanisms, case studies, and checklists live in a skill
// (~/.claude/skills/<name>/) that the agent reads on demand. The prompt tells
// the agent the skill exists; the skill's SKILL.md indexes the references.
//
// Scope is deliberately narrow: only static, stack-specific guidance lives here.
// Cross-stack framing and session-environment context (how the session was
// created, which tools are enabled, the preview environment) belong to a future
// dynamic layer that composes with this output — that context is runtime-derived
// and is not baked into these packs.
package guidance

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
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
// relative to the HOME directory — the path Claude Code scans for personal
// skills, and where sprite images already ship their bundled skills.
// prompt.md points the agent at it by name. Longer-term these skills are
// meant to be published and versioned as a standalone package (npx skills);
// embedding + materializing from the clank binary is the interim mechanism.
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

// InstallSkills materializes the detected stack's playbook into the user's
// personal skills directory (~/.claude/skills) so the agent can read it on
// demand. HOME rather than the worktree keeps the files out of the user's
// repo entirely (no git-exclude dance) and matches where sprite images ship
// their bundled skills. Idempotent — files are overwritten on every call, so
// a clank upgrade refreshes stale copies. No-op for unknown stacks.
func InstallSkills(workDir string) error {
	switch DetectStack(workDir) {
	case StackExpo:
		return installSkillFiles(expoSkillRelDir, expoSkillFiles)
	default:
		return nil
	}
}

// installSkillFiles writes the embedded paths (flattened to their base names)
// under $HOME/relDir.
func installSkillFiles(relDir string, srcPaths []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("guidance: resolve home dir: %w", err)
	}
	dst := filepath.Join(home, filepath.FromSlash(relDir))
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
		if err := writeFileAtomic(dst, path.Base(p), data); err != nil {
			return fmt.Errorf("guidance: write skill doc: %w", err)
		}
	}
	return nil
}

// writeFileAtomic writes data to dir/name via a temp file + rename. Every
// session create/resume fires InstallSkills in its own goroutine (see
// installGuidanceSkills in internal/host), all targeting the same shared
// dir — a plain os.WriteFile lets one goroutine's truncate land between
// another's open and write, tearing the file. Each writer gets a uniquely
// named temp file, so concurrent installs of identical embedded content
// converge safely without needing to serialize them.
func writeFileAtomic(dir, name string, data []byte) error {
	tmp, err := os.CreateTemp(dir, name+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, name))
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
