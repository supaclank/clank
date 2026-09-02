package clankcli

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/supaclank/clank/internal/agent"
	"github.com/supaclank/clank/internal/daemonclient"
	"github.com/supaclank/clank/internal/host"
	"github.com/supaclank/clank/internal/host/hosttest"
	hostmux "github.com/supaclank/clank/internal/host/mux"
	hoststore "github.com/supaclank/clank/internal/host/store"
)

func newCodexAttachTestHost(t *testing.T) (*daemonclient.Client, agent.SessionSnapshot) {
	t.Helper()
	projectDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session := agent.SessionSnapshot{
		ID: "00000000-0000-4000-8000-000000000001", Backend: agent.BackendCodex,
		Title: "Codex app session", Directory: projectDir, UpdatedAt: time.Now(),
	}
	mgr := hosttest.NewCodexDiscoveryManager(t, []agent.SessionSnapshot{session})
	st, err := hoststore.Open(filepath.Join(t.TempDir(), "host.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{agent.BackendCodex: mgr},
		SessionsStore:   st,
	})
	t.Cleanup(svc.Shutdown)
	srv := httptest.NewServer(hostmux.New(svc, nil).Handler())
	t.Cleanup(srv.Close)
	return daemonclient.NewTCPClient(srv.URL, ""), session
}

func TestIntegration_ResolveAttachSessionByID_RediscoversCodex(t *testing.T) {
	client, want := newCodexAttachTestHost(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	got, err := resolveAttachSessionByID(ctx, client, want.ID, want.Directory)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExternalID != want.ID || got.Backend != agent.BackendCodex {
		t.Fatalf("attached %+v, want Codex session %s", got, want.ID)
	}
}

// codexRediscoverProgramTimeout bounds the whole program, including a real
// Codex ACP discovery call (up to sessionPickerDiscoverTimeout's 60s in
// internal/tui) triggered by the "Rediscover sessions" keystep — wider than
// connectProgramTimeout so that call isn't cut off mid-flight.
const codexRediscoverProgramTimeout = 90 * time.Second

func TestIntegration_SessionPickerProgram_RediscoversCodex(t *testing.T) {
	client, want := newCodexAttachTestHost(t)
	result, rendered := runSessionPickerProgramWithTimeout(t, client, want.Directory, codexRediscoverProgramTimeout,
		keyStep{UntilVisible: "Rediscover sessions", Keys: "\r"},
		keyStep{UntilVisible: want.Title, Keys: "\r"})
	sessions, err := client.Sessions().List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	picked := sessionMatchingID(sessions, result.SessionID)
	if picked == nil || picked.ExternalID != want.ID || picked.Backend != agent.BackendCodex {
		t.Fatalf("picked %+v, want Codex session %s\n%s", picked, want.ID, rendered)
	}
}
