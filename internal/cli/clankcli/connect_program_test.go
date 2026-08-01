package clankcli

// Runs the connect UI as a live bubbletea program against a real host
// catalog — Init → fetch → render → keystroke → quit. The model's state
// machine is unit-tested in internal/tui; what's covered here is that it
// behaves as a running program driven by real provider data.
//
// The program is constructed here rather than through showConnectUI so
// the test can supply a window size: a TTY-less run reports no terminal
// dimensions and bubbletea renders every frame into zero columns.

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/acksell/clank/internal/agent"
	daemonclient "github.com/acksell/clank/internal/daemonclient"
	"github.com/acksell/clank/internal/host"
	"github.com/acksell/clank/internal/tui"
)

// connectProgramTimeout bounds a run whose gating condition never comes
// true, so a picker that fails to render fails the test instead of
// hanging it.
const connectProgramTimeout = 20 * time.Second

// runConnectProgram drives the connect UI through steps — each one's
// keys held back until its screen appears — and returns the result plus
// everything rendered. The final step must quit, or the run dies on the
// context deadline.
func runConnectProgram(t *testing.T, client *daemonclient.Client, backend agent.BackendType, steps ...keyStep) (tui.ConnectResult, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), connectProgramTimeout)
	defer cancel()

	out := &syncBuffer{}
	in := &gatedKeys{steps: steps, done: ctx.Done(), visible: out.String}

	model := tui.NewConnectModel(client.Host(host.HostLocal), backend)
	program := tea.NewProgram(model,
		tea.WithContext(ctx),
		tea.WithInput(in),
		tea.WithOutput(out),
		tea.WithWindowSize(100, 40),
	)
	if _, err := program.Run(); err != nil {
		t.Fatalf("connect program: %v\nrendered:\n%s", err, out.String())
	}
	return model.Result(), out.String()
}

// The picker must load the real catalog, render a row per backend with
// its connection state, and quit on a keystroke.
func TestConnectProgram_RendersPickerAndQuits(t *testing.T) {
	client := newConnectTestClient(t)

	result, rendered := runConnectProgram(t, client, "",
		keyStep{UntilVisible: backendLabelFor(agent.BackendCodex), Keys: "q"})

	if result.IsConnected {
		t.Error("quitting the picker must not report a connection")
	}
	for _, bt := range agent.AllBackends {
		if !strings.Contains(rendered, backendLabelFor(bt)) {
			t.Errorf("picker never rendered a row for %s:\n%s", bt, rendered)
		}
	}
	if !strings.Contains(rendered, "not connected") {
		t.Errorf("picker rendered no connection state:\n%s", rendered)
	}
}

// Enter on a picker row hands off to that backend's provider list — the
// real one, fetched from the host. This handoff is the whole reason the
// picker exists.
func TestConnectProgram_PickingABackendLoadsItsProviders(t *testing.T) {
	client := newConnectTestClient(t)

	// enter picks the first row; esc leaves the provider list it opens.
	result, rendered := runConnectProgram(t, client, "",
		keyStep{UntilVisible: backendLabelFor(agent.BackendCodex), Keys: "\r"},
		keyStep{UntilVisible: "GitHub Copilot", Keys: "\x1b"})

	want := agent.AllBackends[0]
	if result.Backend != want {
		t.Errorf("Result().Backend = %q, want the picked %q", result.Backend, want)
	}
	if !strings.Contains(rendered, "Connect Provider") {
		t.Errorf("enter did not open the provider flow:\n%s", rendered)
	}
	if !strings.Contains(rendered, "GitHub Copilot") {
		t.Errorf("provider list did not render the host's real %s catalog:\n%s", want, rendered)
	}
}

// `clank connect claude` must land straight in Anthropic's providers —
// no backend question, and no other backend's providers in the list.
func TestConnectProgram_NamedBackendOpensItsProvidersDirectly(t *testing.T) {
	client := newConnectTestClient(t)

	_, rendered := runConnectProgram(t, client, agent.BackendClaudeCode,
		keyStep{UntilVisible: "Anthropic", Keys: "\x1b"})

	if strings.Contains(rendered, "Which agent do you want to use?") {
		t.Errorf("naming a backend must skip the picker:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Anthropic") {
		t.Errorf("claude-code's providers were not listed:\n%s", rendered)
	}
	if strings.Contains(rendered, "GitHub Copilot") {
		t.Errorf("opencode's providers leaked into a claude-code connect:\n%s", rendered)
	}
}

// syncBuffer is an io.Writer safe to read from the test goroutine while
// bubbletea's renderer writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// keyStep is one gated keystroke: send Keys once UntilVisible has been
// rendered.
type keyStep struct {
	UntilVisible string
	Keys         string
}

// gatedKeys withholds each step's keystrokes until the frame they answer
// is on screen. A plain strings.Reader is drained before the first
// render, so every key would land during the catalog fetch — quitting
// before anything appeared. This is also what sequences a flow: step N+1
// waits for the screen step N produced.
type gatedKeys struct {
	steps   []keyStep
	next    int
	visible func() string
	done    <-chan struct{}
}

func (g *gatedKeys) Read(p []byte) (int, error) {
	if g.next >= len(g.steps) {
		<-g.done // nothing more to send; unblock only on shutdown
		return 0, io.EOF
	}
	step := g.steps[g.next]
	for !strings.Contains(g.visible(), step.UntilVisible) {
		select {
		case <-g.done:
			return 0, io.EOF
		case <-time.After(2 * time.Millisecond):
		}
	}
	g.next++
	return copy(p, step.Keys), nil
}

// backendLabelFor is the picker's display label. Spelled out here rather
// than imported: the assertion is about what the user reads.
func backendLabelFor(bt agent.BackendType) string {
	switch bt {
	case agent.BackendClaudeCode:
		return "Claude Code"
	case agent.BackendOpenCode:
		return "OpenCode"
	case agent.BackendCodex:
		return "Codex"
	default:
		return string(bt)
	}
}
