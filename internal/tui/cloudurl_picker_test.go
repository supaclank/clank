package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/acksell/clank/internal/cloud"
)

// TestCloudURLPicker_ListNavigation verifies cursor movement within the
// provider list and that the Custom row is always the last entry.
func TestCloudURLPicker_ListNavigation(t *testing.T) {
	t.Parallel()

	m := newCloudURLPicker("")

	if m.phase != cloudURLPhaseList {
		t.Fatalf("expected cloudURLPhaseList initially, got %v", m.phase)
	}
	if m.cursor != 0 {
		t.Fatalf("expected cursor at 0, got %d", m.cursor)
	}

	// Move down to the Custom row (last entry).
	for i := 0; i < len(knownCloudProviders); i++ {
		m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	if m.cursor != len(knownCloudProviders) {
		t.Errorf("cursor should be on Custom row (%d), got %d", len(knownCloudProviders), m.cursor)
	}

	// Cannot move further down past the last row.
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if m.cursor != len(knownCloudProviders) {
		t.Errorf("cursor should not go past last row, got %d", m.cursor)
	}

	// Cursor stays at 0 when trying to move up past the first row.
	m.cursor = 0
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if m.cursor != 0 {
		t.Errorf("cursor should not go below 0, got %d", m.cursor)
	}
}

// TestCloudURLPicker_EnterOnKnownProviderEmitsResult verifies that
// pressing Enter on a known provider row emits cloudURLPickerResultMsg
// with that provider's URL.
func TestCloudURLPicker_EnterOnKnownProviderEmitsResult(t *testing.T) {
	t.Parallel()

	if len(knownCloudProviders) == 0 {
		t.Skip("no known cloud providers defined")
	}

	m := newCloudURLPicker("")
	// cursor starts at 0, which is the first known provider.
	m, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected cmd on Enter")
	}
	msg := cmd()
	res, ok := msg.(cloudURLPickerResultMsg)
	if !ok {
		t.Fatalf("expected cloudURLPickerResultMsg, got %T", msg)
	}
	if res.url != knownCloudProviders[0].URL {
		t.Errorf("expected %q, got %q", knownCloudProviders[0].URL, res.url)
	}
	_ = m
}

// TestCloudURLPicker_SelectCustomTransitionsToInput verifies that choosing
// the "Custom URL…" row switches the phase to cloudURLPhaseInput.
func TestCloudURLPicker_SelectCustomTransitionsToInput(t *testing.T) {
	t.Parallel()

	m := newCloudURLPicker("")
	// Move cursor to the Custom row.
	for i := 0; i < len(knownCloudProviders); i++ {
		m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.phase != cloudURLPhaseInput {
		t.Errorf("expected cloudURLPhaseInput after selecting Custom, got %v", m.phase)
	}
}

// TestCloudURLPicker_InputEscReturnsToList verifies Esc in the text-input
// phase returns to the list without cancelling the whole picker.
func TestCloudURLPicker_InputEscReturnsToList(t *testing.T) {
	t.Parallel()

	m := newCloudURLPicker("")
	// Navigate to Custom.
	for i := 0; i < len(knownCloudProviders); i++ {
		m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.phase != cloudURLPhaseInput {
		t.Fatalf("precondition: expected input phase, got %v", m.phase)
	}

	m, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.phase != cloudURLPhaseList {
		t.Errorf("Esc in input should return to list, got phase %v", m.phase)
	}
	if cmd != nil {
		msg := cmd()
		if _, ok := msg.(cloudURLPickerCancelMsg); ok {
			t.Error("Esc in input phase should not cancel the whole picker")
		}
	}
}

// TestCloudURLPicker_ListEscEmitsCancel verifies Esc in the list phase
// emits a cloudURLPickerCancelMsg.
func TestCloudURLPicker_ListEscEmitsCancel(t *testing.T) {
	t.Parallel()

	m := newCloudURLPicker("")
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("expected cmd on Esc")
	}
	msg := cmd()
	if _, ok := msg.(cloudURLPickerCancelMsg); !ok {
		t.Errorf("expected cloudURLPickerCancelMsg, got %T", msg)
	}
}

// TestCloudURLPicker_InputEmptyEnterEmitsResult verifies that pressing Enter
// with an empty URL emits a result with an empty URL (disabling cloud).
func TestCloudURLPicker_InputEmptyEnterEmitsResult(t *testing.T) {
	t.Parallel()

	m := newCloudURLPicker("")
	// Navigate to Custom and enter input phase.
	for i := 0; i < len(knownCloudProviders); i++ {
		m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	m.input.SetValue("")
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected cmd on Enter with empty input")
	}
	msg := cmd()
	res, ok := msg.(cloudURLPickerResultMsg)
	if !ok {
		t.Fatalf("expected cloudURLPickerResultMsg, got %T", msg)
	}
	if res.url != "" {
		t.Errorf("expected empty URL result, got %q", res.url)
	}
}

// TestCloudURLPicker_PreFilledWithCurrentURL verifies the text input is
// seeded with the currently configured URL so the user can edit rather
// than retype.
func TestCloudURLPicker_PreFilledWithCurrentURL(t *testing.T) {
	t.Parallel()

	m := newCloudURLPicker("https://existing.example.com")
	if m.input.Value() != "https://existing.example.com" {
		t.Errorf("expected input pre-filled, got %q", m.input.Value())
	}
}

// TestCloudURLPicker_ViewContainsProviders verifies the list view renders
// each known provider label.
func TestCloudURLPicker_ViewContainsProviders(t *testing.T) {
	t.Parallel()

	m := newCloudURLPicker("")
	out := m.View()
	for _, p := range knownCloudProviders {
		if !strings.Contains(out, p.Name) {
			t.Errorf("expected provider name %q in view", p.Name)
		}
	}
	if !strings.Contains(out, customCloudProviderLabel) {
		t.Errorf("expected %q in view", customCloudProviderLabel)
	}
}

// TestCloudURLPicker_EnterNotConfiguredEmitsOpenMsg verifies that pressing
// Enter in the not-configured phase opens the URL picker.
func TestCloudURLPicker_EnterNotConfiguredEmitsOpenMsg(t *testing.T) {
	t.Parallel()

	m := newCloudView()
	m.phase = cloudPhaseNotConfigured

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected cmd on Enter in not-configured phase")
	}
	msg := cmd()
	if _, ok := msg.(cloudOpenURLPickerMsg); !ok {
		t.Errorf("expected cloudOpenURLPickerMsg, got %T", msg)
	}
}

// TestCloudURLPicker_EnterSignedOutStartsLogin verifies that Enter in
// the signed-out phase starts the PKCE login (not the URL picker).
func TestCloudURLPicker_EnterSignedOutStartsLogin(t *testing.T) {
	t.Parallel()

	m := newCloudView()
	m.phase = cloudPhaseSignedOut
	m.gatewayURL = "https://example.com"
	m.client = cloud.New(m.gatewayURL, nil)

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	// PKCE login transitions to cloudPhaseLoggingIn immediately; we
	// don't run the actual flow in a unit test (it would block on a
	// browser callback). The phase shift is the observable contract.
	if updated.phase != cloudPhaseLoggingIn {
		t.Errorf("expected cloudPhaseLoggingIn after Enter in signed-out, got %v", updated.phase)
	}
	if updated.loginCancel == nil {
		t.Error("expected loginCancel to be set so c/esc can abort")
	}
	// Cancel the in-flight context immediately so the goroutine
	// doesn't leak past the test.
	if updated.loginCancel != nil {
		updated.loginCancel()
	}
}
