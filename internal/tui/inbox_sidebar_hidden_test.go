package tui

import (
	"sync"
	"testing"

	"github.com/supaclank/clank/internal/config"
)

// TestToggleSidebar_InverseProperty guards the contract documented on
// toggleSidebar: two consecutive toggles return both visibility and pane
// focus to their original state.
func TestToggleSidebar_InverseProperty(t *testing.T) {
	t.Parallel()

	m := &InboxModel{pane: paneSidebar, sidebar: SidebarModel{}}
	m.toggleSidebar()
	if !m.sidebarHidden {
		t.Fatal("first toggle: sidebarHidden = false, want true")
	}
	m.toggleSidebar()
	if m.sidebarHidden {
		t.Error("second toggle: sidebarHidden = true, want false")
	}
	if m.pane != paneSidebar {
		t.Errorf("pane after double toggle: got %v, want paneSidebar", m.pane)
	}
}

// TestPersistSidebarHidden_RoundTrip verifies the toggle's persist helper
// writes preferences that a fresh load (as NewInboxModel does) sees.
func TestPersistSidebarHidden_RoundTrip(t *testing.T) {
	// Not t.Parallel: CLANK_DIR is process-global.
	t.Setenv("CLANK_DIR", t.TempDir())

	persistSidebarHidden(true)
	prefs, err := config.LoadPreferences()
	if err != nil {
		t.Fatalf("LoadPreferences: %v", err)
	}
	if !prefs.SidebarHidden {
		t.Error("SidebarHidden after persist(true): got false, want true")
	}

	persistSidebarHidden(false)
	prefs, err = config.LoadPreferences()
	if err != nil {
		t.Fatalf("LoadPreferences: %v", err)
	}
	if prefs.SidebarHidden {
		t.Error("SidebarHidden after persist(false): got true, want false")
	}
}

// TestPersistHelpers_ConcurrentDifferentFields_NoClobber pins that
// concurrent persist helpers touching different Preferences fields never
// lose each other's write. Every helper must serialize through
// config.UpdatePreferences: a raw LoadPreferences+SavePreferences pair can
// load before, and save after, a concurrent updater — silently reverting
// its write.
func TestPersistHelpers_ConcurrentDifferentFields_NoClobber(t *testing.T) {
	// Not t.Parallel: CLANK_DIR is process-global.
	t.Setenv("CLANK_DIR", t.TempDir())
	m := &InboxModel{}

	for i := 0; i < 100; i++ {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			persistSidebarHidden(true)
		}()
		go func() {
			defer wg.Done()
			m.persistSidebarWidthRatio(42)
		}()
		wg.Wait()

		prefs, err := config.LoadPreferences()
		if err != nil {
			t.Fatalf("iteration %d: LoadPreferences: %v", i, err)
		}
		if !prefs.SidebarHidden || prefs.SidebarWidthRatio != 42 {
			t.Fatalf("iteration %d: concurrent persist helpers clobbered each other: %+v", i, prefs)
		}
	}
}
