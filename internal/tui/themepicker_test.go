package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// TestThemePicker_StartsOnCurrentScheme verifies opening the picker with
// a known scheme places the cursor on that scheme so the user can confirm
// it with Enter without first scrolling.
func TestThemePicker_StartsOnCurrentScheme(t *testing.T) {
	t.Cleanup(func() { applyColorScheme(builtInSchemes[0]) })

	p := newThemePicker("dracula")
	wantIdx := -1
	for i, s := range builtInSchemes {
		if s.Name == "dracula" {
			wantIdx = i
		}
	}
	if p.cursor != wantIdx {
		t.Errorf("cursor: got %d, want %d (dracula)", p.cursor, wantIdx)
	}
}

// TestThemePicker_DownArrowAppliesPreview is the key UX contract for the
// picker: moving the cursor must live-apply the hovered scheme.
func TestThemePicker_DownArrowAppliesPreview(t *testing.T) {
	t.Cleanup(func() { applyColorScheme(builtInSchemes[0]) })

	p := newThemePicker(builtInSchemes[0].Name) // starts at index 0
	// After open the default scheme is applied.

	p, _ = p.Update(tea.KeyPressMsg{Code: tea.KeyDown})

	expected := builtInSchemes[p.cursor]
	if got, want := primaryColor, lipgloss.Color(expected.Primary); got != want {
		t.Errorf("palette not applied on preview: primaryColor=%v, want %v", got, want)
	}
}

// TestThemePicker_EnterReturnsResult checks that Enter emits the result
// message carrying the selected scheme name.
func TestThemePicker_EnterReturnsResult(t *testing.T) {
	t.Cleanup(func() { applyColorScheme(builtInSchemes[0]) })

	p := newThemePicker(builtInSchemes[0].Name)
	p, _ = p.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	hovered := builtInSchemes[p.cursor].Name

	_, cmd := p.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected cmd on Enter")
	}
	res, ok := cmd().(themePickerResultMsg)
	if !ok {
		t.Fatalf("expected themePickerResultMsg, got %T", cmd())
	}
	if res.scheme != hovered {
		t.Errorf("result scheme: got %q, want %q", res.scheme, hovered)
	}
}

// TestThemePicker_EscRevertsAndCancels is the critical "live preview
// doesn't leak" contract: if the user opens the picker, hovers a new
// scheme, then hits Esc, the palette must be restored to whatever was
// active before the picker opened.
func TestThemePicker_EscRevertsAndCancels(t *testing.T) {
	t.Cleanup(func() { applyColorScheme(builtInSchemes[0]) })

	// Start with "dracula" applied so we can tell if Esc restores it.
	drac, _ := schemeByName("dracula")
	applyColorScheme(drac)

	p := newThemePicker("dracula")
	p, _ = p.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // preview a different scheme
	previewedPrimary := primaryColor
	if previewedPrimary == lipgloss.Color(drac.Primary) {
		t.Fatal("preview did not change the palette; test setup is broken")
	}

	_, cmd := p.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("expected cmd on Esc")
	}
	if _, ok := cmd().(themePickerCancelMsg); !ok {
		t.Fatalf("expected themePickerCancelMsg, got %T", cmd())
	}
	if got, want := primaryColor, lipgloss.Color(drac.Primary); got != want {
		t.Errorf("Esc did not revert palette: primaryColor=%v, want %v", got, want)
	}
}

// TestThemePicker_DownAtBottomNoop guards against an index-out-of-range
// when the cursor is already on the last scheme.
func TestThemePicker_DownAtBottomNoop(t *testing.T) {
	t.Cleanup(func() { applyColorScheme(builtInSchemes[0]) })

	p := newThemePicker(builtInSchemes[len(builtInSchemes)-1].Name)
	startIdx := p.cursor
	p, _ = p.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if p.cursor != startIdx {
		t.Errorf("cursor moved past last entry: got %d, want %d", p.cursor, startIdx)
	}
}

// TestThemePicker_UpAtTopNoop mirrors the above for the first scheme.
func TestThemePicker_UpAtTopNoop(t *testing.T) {
	t.Cleanup(func() { applyColorScheme(builtInSchemes[0]) })

	p := newThemePicker(builtInSchemes[0].Name)
	p, _ = p.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if p.cursor != 0 {
		t.Errorf("cursor moved above first entry: got %d, want 0", p.cursor)
	}
}
