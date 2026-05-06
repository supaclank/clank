package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/acksell/clank/internal/agent"
)

func sampleModels(n int) []agent.ModelInfo {
	out := make([]agent.ModelInfo, n)
	for i := 0; i < n; i++ {
		out[i] = agent.ModelInfo{
			ID:           "m-" + string(rune('a'+i)),
			Name:         "Model " + string(rune('a'+i)),
			ProviderID:   "openai",
			ProviderName: "OpenAI",
		}
	}
	return out
}

// TestModelPicker_OpenCodeIncludesConnectProviderEntry pins that the
// synthetic "+ Connect provider…" row is present for OpenCode (where
// the auth flow makes sense). Without this entry users with a broken or
// missing auth.json have no in-picker affordance to fix it — the old
// hint just told them to leave clank and run opencode CLI.
func TestModelPicker_OpenCodeIncludesConnectProviderEntry(t *testing.T) {
	t.Parallel()
	p := newModelPicker(sampleModels(3), -1, agent.BackendOpenCode)
	var found bool
	for _, item := range p.items {
		if item.index == modelPickerIndexConnectProvider {
			found = true
			break
		}
	}
	if !found {
		t.Error("OpenCode picker missing the Connect provider entry")
	}
}

func TestModelPicker_ClaudeCodeOmitsConnectProviderEntry(t *testing.T) {
	t.Parallel()
	p := newModelPicker(sampleModels(3), -1, agent.BackendClaudeCode)
	for _, item := range p.items {
		if item.index == modelPickerIndexConnectProvider {
			t.Errorf("Claude Code picker should not show Connect provider; entry has index %d", item.index)
		}
	}
}

// TestModelPicker_EnterOnConnectProvider_EmitsConnectMsg verifies that
// activating the synthetic row emits modelPickerConnectProviderMsg, not
// modelPickerResultMsg. The session view branches on this to bubble up
// to the inbox's settings + provider-auth modal.
func TestModelPicker_EnterOnConnectProvider_EmitsConnectMsg(t *testing.T) {
	t.Parallel()
	p := newModelPicker(sampleModels(2), -1, agent.BackendOpenCode)
	// Move cursor to the last filtered item (the Connect provider row
	// is appended after the models).
	for i, item := range p.filtered {
		if item.index == modelPickerIndexConnectProvider {
			p.cursor = i
			break
		}
	}
	_, cmd := p.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter on Connect-provider produced no command")
	}
	msg := cmd()
	if _, ok := msg.(modelPickerConnectProviderMsg); !ok {
		t.Errorf("expected modelPickerConnectProviderMsg, got %T", msg)
	}
}

// TestModelPicker_MouseWheelScrolls verifies trackpad scroll moves the
// cursor in the chat view's mouse-capture mode. The chat view enables
// cell-motion mouse capture, so wheel events route to bubbletea instead
// of the terminal's own scroll buffer; without explicit handling the
// picker silently ignored them.
func TestModelPicker_MouseWheelScrolls(t *testing.T) {
	t.Parallel()
	p := newModelPicker(sampleModels(20), -1, agent.BackendOpenCode)
	startCursor := p.cursor

	p, _ = p.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if p.cursor != startCursor+1 {
		t.Errorf("wheel-down: cursor %d, want %d", p.cursor, startCursor+1)
	}

	p, _ = p.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if p.cursor != startCursor {
		t.Errorf("wheel-up: cursor %d, want %d", p.cursor, startCursor)
	}
}

// TestModelPicker_HintReferencesConnectProviderEntry pins the hint text
// so a future copy edit doesn't silently revert it to the old "ctrl+p
// in opencode" message. The picker is the user's path to provider auth
// — the hint must surface that.
func TestModelPicker_HintReferencesConnectProviderEntry(t *testing.T) {
	t.Parallel()
	p := newModelPicker(sampleModels(2), -1, agent.BackendOpenCode)
	view := p.View()
	if strings.Contains(view, "ctrl+p in opencode") {
		t.Error("picker still shows the stale 'ctrl+p in opencode' hint")
	}
	if !strings.Contains(view, "Connect provider") {
		t.Errorf("picker hint should reference the Connect provider entry; got:\n%s", view)
	}
}

// TestModelPicker_ConnectProviderSurvivesFilter pins the contract that
// the Connect-provider row is pinned to the bottom of the filtered
// results regardless of query. The whole point of the entry is to
// rescue users whose search returns nothing because their auth.json
// is missing or stale; filtering it out on a non-matching query
// defeats that.
func TestModelPicker_ConnectProviderSurvivesFilter(t *testing.T) {
	t.Parallel()
	p := newModelPicker(sampleModels(3), -1, agent.BackendOpenCode)

	// A query that matches none of the model IDs ("m-a", "m-b", "m-c").
	p.search.SetValue("nothingmatches")
	p.applyFilter()

	if len(p.filtered) == 0 {
		t.Fatalf("filter wiped everything; Connect-provider entry should remain")
	}
	last := p.filtered[len(p.filtered)-1]
	if last.index != modelPickerIndexConnectProvider {
		t.Errorf("Connect-provider entry should be pinned to the bottom; got last index %d", last.index)
	}
	// And it should be the only entry, since no models matched.
	if len(p.filtered) != 1 {
		t.Errorf("expected only the Connect-provider entry to survive a non-matching query; got %d entries", len(p.filtered))
	}

	// A query that matches one model should keep that match plus the
	// Connect-provider entry.
	p.search.SetValue("m-b")
	p.applyFilter()
	if len(p.filtered) != 2 {
		t.Fatalf("expected match + Connect-provider; got %d entries: %+v", len(p.filtered), p.filtered)
	}
	if p.filtered[len(p.filtered)-1].index != modelPickerIndexConnectProvider {
		t.Error("Connect-provider entry should still be last after a partial match")
	}
}
