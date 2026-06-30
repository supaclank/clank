package guidance_test

import (
	"os"
	"path/filepath"
	"strings"
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
	// One marker per doc in the pack — also proves every embedded file was read
	// and concatenated (guards readPack against a typo'd embed path).
	for _, marker := range []string{
		"existing Expo",    // intro.md
		"npx expo install", // dependencies.md
		"First principles", // performance.md
		"Safe areas",       // ux.md
	} {
		if !strings.Contains(got, marker) {
			t.Errorf("Assemble output missing marker %q", marker)
		}
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
