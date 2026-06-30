// Package guidance assembles the stack-specific guidance that clank injects as
// the building agent's system prompt at session start. The stack is detected by
// inspecting the project's package.json; today only Expo / React Native is
// recognized, returning "" for anything else.
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
	"os"
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
// guidance. Order matters: framing first, then the actionable rules.
var expoPack = []string{
	"docs/expo/intro.md",
	"docs/expo/dependencies.md",
	"docs/expo/performance.md",
	"docs/expo/ux.md",
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
