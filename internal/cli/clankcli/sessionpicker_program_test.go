package clankcli

// Runs the --attach session picker as a live bubbletea program against a
// real host catalog — Init → list fetch → render → keystroke → quit. The
// model's state machine is unit-tested in internal/tui; what's covered
// here is the program behaving end to end on real session data,
// including the rediscover action registering archive-only sessions.

import (
	"context"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/supaclank/clank/internal/agent"
	daemonclient "github.com/supaclank/clank/internal/daemonclient"
	"github.com/supaclank/clank/internal/host/hosttest"
	"github.com/supaclank/clank/internal/tui"
)

func runSessionPickerProgram(t *testing.T, client *daemonclient.Client, projectDir string, steps ...keyStep) (tui.SessionPickerResult, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), connectProgramTimeout)
	defer cancel()

	out := &syncBuffer{}
	in := &gatedKeys{steps: steps, done: ctx.Done(), visible: out.String}

	model := tui.NewSessionPickerModel(client, projectDir)
	program := tea.NewProgram(model,
		tea.WithContext(ctx),
		tea.WithInput(in),
		tea.WithOutput(out),
		tea.WithWindowSize(100, 40),
	)
	if _, err := program.Run(); err != nil {
		t.Fatalf("session picker program: %v\nrendered:\n%s", err, out.String())
	}
	return model.Result(), out.String()
}

// seedDiscoveredSession registers one archive session with the host via
// the same discovery path the picker's rediscover action uses.
func seedDiscoveredSession(t *testing.T, client *daemonclient.Client, stub *hosttest.StubBackendManager, snap agent.SessionSnapshot) {
	t.Helper()
	stub.SetDiscoverSnapshots([]agent.SessionSnapshot{snap})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Sessions().Discover(ctx, agent.BackendOpenCode, snap.Directory); err != nil {
		t.Fatalf("seed discover: %v", err)
	}
	stub.SetDiscoverSnapshots(nil)
}

// Enter on the loaded list must attach the most recently active session.
func TestSessionPickerProgram_EnterAttachesNewestSession(t *testing.T) {
	client, stub := newTestHost(t)
	projectDir := t.TempDir()
	seedDiscoveredSession(t, client, stub, agent.SessionSnapshot{
		ID: "ses_old", Backend: agent.BackendOpenCode, Title: "older work",
		Directory: projectDir, UpdatedAt: time.Now().Add(-2 * time.Hour),
	})
	seedDiscoveredSession(t, client, stub, agent.SessionSnapshot{
		ID: "ses_new", Backend: agent.BackendOpenCode, Title: "latest work",
		Directory: projectDir, UpdatedAt: time.Now().Add(-time.Minute),
	})

	result, rendered := runSessionPickerProgram(t, client, projectDir,
		keyStep{UntilVisible: "older work", Keys: "\r"})

	if result.SessionID == "" || result.IsAborted {
		t.Fatalf("result = %+v, want a selection", result)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sessions, err := client.Sessions().List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	picked := sessionMatchingID(sessions, result.SessionID)
	if picked == nil {
		t.Fatalf("picked id %q is not in the catalog", result.SessionID)
	}
	if picked.ExternalID != "ses_new" {
		t.Errorf("picked %q (%s), want the newest session ses_new\nrendered:\n%s", picked.ExternalID, picked.Title, rendered)
	}
}

// The rediscover action must register archive-only sessions and show
// them without restarting the picker.
func TestSessionPickerProgram_RediscoverImportsAndAttaches(t *testing.T) {
	client, stub := newTestHost(t)
	projectDir := t.TempDir()
	stub.SetDiscoverSnapshots([]agent.SessionSnapshot{{
		ID: "ses_hidden", Backend: agent.BackendOpenCode, Title: "not registered yet",
		Directory: projectDir, UpdatedAt: time.Now().Add(-time.Hour),
	}})

	// Empty catalog: the rediscover action is the only row, so enter
	// fires it; the imported session then renders and enter attaches it.
	result, rendered := runSessionPickerProgram(t, client, projectDir,
		keyStep{UntilVisible: "Rediscover sessions", Keys: "\r"},
		keyStep{UntilVisible: "not registered yet", Keys: "\r"})

	if result.SessionID == "" {
		t.Fatalf("nothing attached after rediscovery\nrendered:\n%s", rendered)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sessions, err := client.Sessions().List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	picked := sessionMatchingID(sessions, result.SessionID)
	if picked == nil || picked.ExternalID != "ses_hidden" {
		t.Fatalf("picked %+v, want the rediscovered ses_hidden", picked)
	}
}
