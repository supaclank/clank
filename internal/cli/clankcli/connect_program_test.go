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

	"github.com/supaclank/clank/internal/agent"
	daemonclient "github.com/supaclank/clank/internal/daemonclient"
	"github.com/supaclank/clank/internal/host"
	"github.com/supaclank/clank/internal/tui"
)

// connectProgramTimeout bounds a run whose gating condition never comes
// true, so a picker that fails to render fails the test instead of
// hanging it.
const connectProgramTimeout = 20 * time.Second

// gatedKeyPollInterval paces the wait for a frame to appear. Frames land
// in milliseconds, so this only bounds the response delay — kept coarse
// so several of these programs running beside the rest of the suite
// don't spin a core each.
const gatedKeyPollInterval = 10 * time.Millisecond

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

	// enter picks the first row; ctrl+c ends the run from the provider
	// list (esc would step back to the picker and release the choice —
	// see TestConnectProgram_EscFromProvidersReturnsToPicker).
	result, rendered := runConnectProgram(t, client, "",
		keyStep{UntilVisible: backendLabelFor(agent.BackendCodex), Keys: "\r"},
		keyStep{UntilVisible: "GitHub Copilot", Keys: "\x03"})

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

	// mark is how much output had been written when the previous step
	// fired. Each step matches only what was drawn after it, so a step
	// can wait for a screen the flow has already shown once — returning
	// to the picker renders the same text a second time.
	mark int
}

func (g *gatedKeys) Read(p []byte) (int, error) {
	if g.next >= len(g.steps) {
		<-g.done // nothing more to send; unblock only on shutdown
		return 0, io.EOF
	}
	step := g.steps[g.next]
	for !strings.Contains(g.visible()[g.mark:], step.UntilVisible) {
		select {
		case <-g.done:
			return 0, io.EOF
		case <-time.After(gatedKeyPollInterval):
		}
	}
	g.next++
	g.mark = len(g.visible())
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

// The full round trip a user makes after picking the wrong agent: pick a
// backend, see its providers, back out, and land on the picker again
// with the choice released. A cleared Backend is what distinguishes
// "went back" from "esc quit the program" — a quit would leave the
// picked backend on the result and never process the trailing key.
func TestConnectProgram_EscFromProvidersReturnsToPicker(t *testing.T) {
	client := newConnectTestClient(t)

	result, rendered := runConnectProgram(t, client, "",
		keyStep{UntilVisible: backendLabelFor(agent.BackendCodex), Keys: "\r"},
		keyStep{UntilVisible: "GitHub Copilot", Keys: "\x1b"},
		keyStep{UntilVisible: "Which agent do you want to use?", Keys: "q"})

	if result.Backend != "" {
		t.Errorf("Result().Backend = %q, want cleared — esc quit instead of going back", result.Backend)
	}
	if result.IsConnected {
		t.Error("backing out must not report a connection")
	}
	if !strings.Contains(rendered, "GitHub Copilot") {
		t.Errorf("setup: the provider list never rendered:\n%s", rendered)
	}
}

// q is the one-key exit from a real run: quit straight out of the
// provider list without touching esc or ctrl+c.
func TestConnectProgram_QQuitsFromTheProviderList(t *testing.T) {
	client := newConnectTestClient(t)

	result, rendered := runConnectProgram(t, client, agent.BackendClaudeCode,
		keyStep{UntilVisible: "Anthropic", Keys: "q"})

	if result.IsConnected {
		t.Error("quitting must not report a connection")
	}
	if !strings.Contains(rendered, "Anthropic") {
		t.Errorf("setup: the provider list never rendered:\n%s", rendered)
	}
}

// …but inside the key form q is a character. Typing "q" then quitting
// with ctrl+c proves the letter reached the field instead of ending the
// program — an API key with a q in it has to be enterable.
func TestConnectProgram_QTypesInsideTheKeyForm(t *testing.T) {
	client := newConnectTestClient(t)

	// OpenAI is an api-key provider: enter opens the confirm gate, y
	// opens the key form, then "q" must land in the field.
	_, rendered := runConnectProgram(t, client, agent.BackendOpenCode,
		keyStep{UntilVisible: "OpenAI", Keys: "\x1b[B\r"},
		keyStep{UntilVisible: "will restart the OpenCode server", Keys: "y"},
		keyStep{UntilVisible: "API key", Keys: "q"},
		keyStep{UntilVisible: "•", Keys: "\x03"})

	if !strings.Contains(rendered, "API key") {
		t.Fatalf("setup: never reached the key form:\n%s", rendered)
	}
	// The field masks input, so the typed q shows as a bullet — and the
	// program is still alive to have rendered it.
	if !strings.Contains(rendered, "•") {
		t.Errorf("q did not reach the masked key field:\n%s", rendered)
	}
}
