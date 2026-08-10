package host_test

import (
	"context"
	"maps"
	"path/filepath"
	"testing"
	"time"

	"github.com/supaclank/clank/internal/agent"
	acpx "github.com/supaclank/clank/internal/agent/acp"
	"github.com/supaclank/clank/internal/agent/acp/acptest"
	"github.com/supaclank/clank/internal/host"
	hoststore "github.com/supaclank/clank/internal/host/store"
	sdk "github.com/coder/acp-go-sdk"
)

// TestEnsureBackend_RehydrateReassertsPersistedConfig reproduces the
// hibernate→wake mode reset end-to-end: a session created with
// mode=bypassPermissions whose backend dropped (sprite hibernation,
// daemon restart) is rehydrated by ensureBackend on the next send. The
// fresh agent process boots in its own default mode; the host must pass
// the row's last-applied config into the rebuild so the backend
// re-asserts it — after session/load and before the prompt dispatches.
// Without that, every wake silently downgraded bypassPermissions to
// prompt-mode and unattended runs stalled on permission prompts.
func TestEnsureBackend_RehydrateReassertsPersistedConfig(t *testing.T) {
	// installGuidanceSkills writes under $HOME — isolate it.
	t.Setenv("HOME", t.TempDir())

	st, err := hoststore.Open(filepath.Join(t.TempDir(), "host.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	repo := initGitRepo(t, "git@example.com:acme/repo.git")

	calls := make(chan string, 8)
	scripted := func(string) *acptest.ScriptedAgent {
		a := &acptest.ScriptedAgent{}
		a.LoadSessionFn = func(ctx context.Context, p sdk.LoadSessionRequest) (sdk.LoadSessionResponse, error) {
			calls <- "load:" + string(p.SessionId)
			return sdk.LoadSessionResponse{Modes: &sdk.SessionModeState{
				CurrentModeId: "default",
				AvailableModes: []sdk.SessionMode{
					{Id: "default", Name: "Manual"},
					{Id: "bypassPermissions", Name: "Bypass"},
				},
			}}, nil
		}
		a.SetModeFn = func(ctx context.Context, p sdk.SetSessionModeRequest) (sdk.SetSessionModeResponse, error) {
			calls <- "set_mode:" + string(p.ModeId)
			return sdk.SetSessionModeResponse{}, nil
		}
		a.PromptFn = func(ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
			calls <- "prompt"
			return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
		}
		return a
	}
	mgr, err := host.NewCodexACPManager(acpDirs(t))
	if err != nil {
		t.Fatalf("NewCodexACPManager: %v", err)
	}
	mgr.Supervisor().SetSpawnFunc(acptest.SpawnFunc(scripted, acpx.CodexProfile("unused-bun", "unused-entry", nil), t.Logf))
	mgr.Supervisor().SetReconcileInterval(20 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := mgr.Init(ctx, func() ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer mgr.Shutdown()

	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendCodex: mgr,
		},
		SessionsStore: st,
	})
	t.Cleanup(svc.Shutdown)

	// The persisted session as a hibernated sprite wakes with it: a live
	// external id and the last-applied config, but no live backend.
	const id = "01REHYDRATECONFIG0000001"
	if err := st.UpsertSession(ctx, agent.SessionInfo{
		ID:         id,
		ExternalID: "ext-hibernated-1",
		Backend:    agent.BackendCodex,
		Status:     agent.StatusIdle,
		GitRef:     agent.GitRef{LocalPath: repo},
		Config:     map[string]string{agent.ConfigOptionMode: "bypassPermissions"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := svc.SendMessage(ctx, id, agent.SendMessageOpts{Text: "continue the run"}); err != nil {
		t.Fatalf("SendMessage (rehydrate): %v", err)
	}

	for _, want := range []string{"load:ext-hibernated-1", "set_mode:bypassPermissions", "prompt"} {
		select {
		case got := <-calls:
			if got != want {
				t.Fatalf("agent call = %q, want %q (mode must be restored after load and before the prompt)", got, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("agent never received %q", want)
		}
	}
}

// Config changes dispatched on the send path must merge into the row's
// last-applied config (DATA-040: omitted keys mean "no change"), without
// bumping UpdatedAt — recording policy is not agent activity, and the
// inbox sorts on UpdatedAt.
func TestSendMessage_RecordsAppliedConfigOnRow(t *testing.T) {
	t.Parallel()
	st, err := hoststore.Open(filepath.Join(t.TempDir(), "host.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	repo := initGitRepo(t, "git@example.com:acme/repo.git")

	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &rehydrateBackendManager{},
		},
		SessionsStore: st,
	})
	t.Cleanup(svc.Shutdown)

	ctx := context.Background()
	const id = "01RECORDCONFIG0000000001"
	seeded := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	if err := st.UpsertSession(ctx, agent.SessionInfo{
		ID:        id,
		Backend:   agent.BackendOpenCode,
		Status:    agent.StatusIdle,
		GitRef:    agent.GitRef{LocalPath: repo},
		Config:    map[string]string{agent.ConfigOptionMode: "build", "effort": "default"},
		UpdatedAt: seeded,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := svc.SendMessage(ctx, id, agent.SendMessageOpts{
		Text:   "switch to plan",
		Config: map[string]string{agent.ConfigOptionMode: "plan"},
	}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	info, err := st.GetSession(ctx, id)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	want := map[string]string{agent.ConfigOptionMode: "plan", "effort": "default"}
	if !maps.Equal(info.Config, want) {
		t.Errorf("row config after config-carrying send = %v, want %v (merge, not replace)", info.Config, want)
	}
	if !info.UpdatedAt.Equal(seeded) {
		t.Errorf("UpdatedAt = %v, want untouched %v (recording config must not reorder the inbox)", info.UpdatedAt, seeded)
	}

	// A config-less follow-up changes nothing (omitted = no change).
	if err := svc.SendMessage(ctx, id, agent.SendMessageOpts{Text: "keep going"}); err != nil {
		t.Fatalf("SendMessage (no config): %v", err)
	}
	info, err = st.GetSession(ctx, id)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !maps.Equal(info.Config, want) {
		t.Errorf("row config after config-less send = %v, want unchanged %v", info.Config, want)
	}
}

// CreateSession persists the create-time config on the row — the seed
// the first rehydrate re-asserts.
func TestCreateSession_PersistsConfig(t *testing.T) {
	t.Parallel()
	st, err := hoststore.Open(filepath.Join(t.TempDir(), "host.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	repo := initGitRepo(t, "git@example.com:acme/repo.git")

	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &rehydrateBackendManager{},
		},
		SessionsStore: st,
	})
	t.Cleanup(svc.Shutdown)

	ctx := context.Background()
	cfg := map[string]string{agent.ConfigOptionMode: "bypassPermissions", "model": "default"}
	_, info, err := svc.CreateSession(ctx, "01CREATECONFIG0000000001", agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{LocalPath: repo},
		Prompt:  "go",
		Config:  cfg,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !maps.Equal(info.Config, cfg) {
		t.Errorf("returned info config = %v, want %v", info.Config, cfg)
	}
	stored, err := st.GetSession(ctx, info.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !maps.Equal(stored.Config, cfg) {
		t.Errorf("persisted config = %v, want %v", stored.Config, cfg)
	}
}

// modeEmittingBackend is a minimal live backend whose event channel the
// test feeds — the seam for exercising the relay → applyEventToMetadata
// path with an agent-initiated mode change.
type modeEmittingBackend struct {
	noopBackend
	events chan agent.Event
}

func (b *modeEmittingBackend) Events() <-chan agent.Event { return b.events }
func (b *modeEmittingBackend) Status() agent.SessionStatus {
	return agent.StatusIdle
}

type modeEmittingBackendManager struct{ backend *modeEmittingBackend }

func (m *modeEmittingBackendManager) Init(_ context.Context, _ func() ([]string, error)) error {
	return nil
}
func (m *modeEmittingBackendManager) CreateBackend(_ context.Context, _ agent.BackendInvocation) (agent.SessionBackend, error) {
	return m.backend, nil
}
func (m *modeEmittingBackendManager) Shutdown() {}

// An EventModeChange from the backend (agent-initiated flip, e.g. plan
// approval) must fold into the row's persisted config — otherwise the
// next rehydrate would re-assert the stale pre-approval mode.
func TestModeChangeEvent_FoldsIntoPersistedConfig(t *testing.T) {
	t.Parallel()
	st, err := hoststore.Open(filepath.Join(t.TempDir(), "host.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	repo := initGitRepo(t, "git@example.com:acme/repo.git")

	backend := &modeEmittingBackend{events: make(chan agent.Event, 4)}
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &modeEmittingBackendManager{backend: backend},
		},
		SessionsStore: st,
	})
	t.Cleanup(svc.Shutdown)

	ctx := context.Background()
	const id = "01MODEEVENT0000000000001"
	_, _, err = svc.CreateSession(ctx, id, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{LocalPath: repo},
		Prompt:  "plan it",
		Config:  map[string]string{agent.ConfigOptionMode: "plan"},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	backend.events <- agent.Event{Type: agent.EventModeChange, Data: agent.ModeChangeData{ModeID: "build"}}

	deadline := time.Now().Add(5 * time.Second)
	for {
		info, err := st.GetSession(ctx, id)
		if err != nil {
			t.Fatalf("GetSession: %v", err)
		}
		if info.Config[agent.ConfigOptionMode] == "build" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("persisted config mode = %q, want the agent-flipped build", info.Config[agent.ConfigOptionMode])
		}
		time.Sleep(10 * time.Millisecond)
	}
}
