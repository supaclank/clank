package tui

import (
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/supaclank/clank/internal/agent"
)

func TestImportSessions_IncludesCodex(t *testing.T) {
	t.Parallel()
	m := newImportSessionsModel()
	if !strings.Contains(m.View(), "Codex") {
		t.Fatal("Codex is missing from the rendered import menu")
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	confirmed := cmd().(importSessionsConfirmMsg)
	if !slices.Equal(confirmed.providers, agent.AllBackends) {
		t.Fatalf("default selection = %v, want all backends", confirmed.providers)
	}
	for _, provider := range confirmed.providers {
		if provider == agent.BackendCodex {
			return
		}
	}
	t.Fatal("the default import selection does not include Codex")
}

func TestImportSessions_TogglesEachBackend(t *testing.T) {
	t.Parallel()
	for i, backend := range agent.AllBackends {
		t.Run(string(backend), func(t *testing.T) {
			t.Parallel()
			m := newImportSessionsModel()
			for range i {
				m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
			}
			m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeySpace})
			_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			got := cmd().(importSessionsConfirmMsg).providers
			want := slices.Delete(slices.Clone(agent.AllBackends), i, i+1)
			if !slices.Equal(got, want) {
				t.Fatalf("selection after toggling %s: %v, want %v", backend, got, want)
			}
		})
	}
}
