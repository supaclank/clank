package tui

import (
	"testing"

	"charm.land/lipgloss/v2"
)

// TestSchemeByName_Default verifies that an empty name resolves to the
// first built-in scheme (the documented default).
func TestSchemeByName_Default(t *testing.T) {
	t.Parallel()

	got, ok := schemeByName("")
	if !ok {
		t.Fatal("schemeByName(\"\") returned ok=false; expected default scheme")
	}
	if got.Name != builtInSchemes[0].Name {
		t.Errorf("empty name should resolve to %q, got %q", builtInSchemes[0].Name, got.Name)
	}
}

func TestSchemeByName_KnownScheme(t *testing.T) {
	t.Parallel()

	got, ok := schemeByName("dracula")
	if !ok {
		t.Fatal("expected dracula to be a known scheme")
	}
	if got.Name != "dracula" {
		t.Errorf("got %q, want dracula", got.Name)
	}
}

// TestSchemeByName_Unknown verifies unknown names return ok=false so
// callers can distinguish "no match" from "default".
func TestSchemeByName_Unknown(t *testing.T) {
	t.Parallel()

	if _, ok := schemeByName("does-not-exist"); ok {
		t.Error("expected unknown scheme to return ok=false")
	}
}

// TestApplyColorScheme_MutatesPaletteVars verifies applyColorScheme
// actually swaps the package-level palette vars so downstream render
// code sees the new colors on the next frame.
//
// This mutates global state, so it cannot run in parallel with other
// tests that read palette vars. The test restores the original scheme
// at the end to avoid leaking into sibling tests.
func TestApplyColorScheme_MutatesPaletteVars(t *testing.T) {
	origPrimary := primaryColor
	origText := textColor
	t.Cleanup(func() {
		applyColorScheme(builtInSchemes[0])
		// Sanity: make sure cleanup restored something — useful if we
		// ever accidentally break the default scheme.
		if primaryColor != origPrimary {
			t.Logf("warning: primaryColor not restored to original (%v vs %v)", primaryColor, origPrimary)
		}
		_ = origText
	})

	target, _ := schemeByName("tokyo-night")
	applyColorScheme(target)

	if got, want := primaryColor, lipgloss.Color(target.Primary); got != want {
		t.Errorf("primaryColor: got %v, want %v", got, want)
	}
	if got, want := textColor, lipgloss.Color(target.Text); got != want {
		t.Errorf("textColor: got %v, want %v", got, want)
	}
}

// TestApplyColorScheme_RebuildsCapturedStyles verifies the module-level
// styles (which capture colors at init time) actually pick up the new
// palette after applyColorScheme.
//
// Without rebuildModuleStyles(), selectedStyle would keep the old
// background color forever, producing an off-scheme highlight.
func TestApplyColorScheme_RebuildsCapturedStyles(t *testing.T) {
	t.Cleanup(func() { applyColorScheme(builtInSchemes[0]) })

	target, _ := schemeByName("gruvbox-dark")
	applyColorScheme(target)

	// selectedStyle uses primaryColor as Background; rendering any text
	// should include the ANSI sequence for that color. We don't parse
	// the ANSI output — we just check the style's GetBackground value.
	if got := selectedStyle.GetBackground(); got != lipgloss.Color(target.Primary) {
		t.Errorf("selectedStyle.Background: got %v, want %v", got, lipgloss.Color(target.Primary))
	}
	if got := borderStyle.GetBorderTopForeground(); got != lipgloss.Color(target.Primary) {
		t.Errorf("borderStyle.BorderForeground: got %v, want %v", got, lipgloss.Color(target.Primary))
	}
}

// TestApplySchemeFromPreference_UnknownFallsBackToDefault ensures a
// corrupt preferences file (unknown scheme name) can't brick the TUI:
// we silently use the default scheme.
func TestApplySchemeFromPreference_UnknownFallsBackToDefault(t *testing.T) {
	t.Cleanup(func() { applyColorScheme(builtInSchemes[0]) })

	applySchemeFromPreference("does-not-exist-scheme-name")

	if got, want := primaryColor, lipgloss.Color(builtInSchemes[0].Primary); got != want {
		t.Errorf("primaryColor: got %v, want default %v", got, want)
	}
}

func TestSchemeNames_IncludesAllBuiltIns(t *testing.T) {
	t.Parallel()

	names := schemeNames()
	if len(names) != len(builtInSchemes) {
		t.Fatalf("schemeNames returned %d entries, want %d", len(names), len(builtInSchemes))
	}
	for i, s := range builtInSchemes {
		if names[i] != s.Name {
			t.Errorf("names[%d]: got %q, want %q", i, names[i], s.Name)
		}
	}
}

// TestBuiltInSchemes_DefaultFirst enforces the invariant that index 0 is
// the fallback scheme (several call sites rely on it).
func TestBuiltInSchemes_DefaultFirst(t *testing.T) {
	t.Parallel()

	if builtInSchemes[0].Name != "gruvbox-dark" {
		t.Errorf("builtInSchemes[0] must be named 'gruvbox-dark', got %q", builtInSchemes[0].Name)
	}
}

// TestBuiltInSchemes_AllFieldsPopulated catches accidentally-empty palette
// entries (which would render as the terminal's default foreground and
// make the scheme look broken in places).
func TestBuiltInSchemes_AllFieldsPopulated(t *testing.T) {
	t.Parallel()

	for _, s := range builtInSchemes {
		if s.Primary == "" || s.Secondary == "" || s.Success == "" ||
			s.Warning == "" || s.Danger == "" || s.Muted == "" ||
			s.Text == "" || s.Dim == "" || s.Draft == "" {
			t.Errorf("scheme %q has empty color field(s): %+v", s.Name, s)
		}
	}
}
