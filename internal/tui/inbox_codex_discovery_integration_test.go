package tui

import (
	"net/http/httptest"
	"os"
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

func TestIntegration_InboxDiscoversCodex(t *testing.T) {
	for _, scope := range []string{"refresh", "selected-provider"} {
		t.Run(scope, func(t *testing.T) {
			dir, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			want := agent.SessionSnapshot{
				ID: "00000000-0000-4000-8000-000000000001", Backend: agent.BackendCodex,
				Title: "Codex app session", Directory: dir, UpdatedAt: time.Now(),
			}
			mgr := hosttest.NewCodexDiscoveryManager(t, []agent.SessionSnapshot{want})
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
			client := daemonclient.NewTCPClient(srv.URL, "")
			m := &InboxModel{client: client}
			if scope == "refresh" {
				m.discoverCmd()()
			} else {
				result := m.discoverForProvidersCmd([]agent.BackendType{agent.BackendCodex})().(discoverResultMsg)
				if result.err != nil || result.imported != 1 {
					t.Fatalf("import result: %+v", result)
				}
			}
			sessions, err := client.Sessions().List(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if len(sessions) != 1 || sessions[0].ExternalID != want.ID || sessions[0].Backend != agent.BackendCodex {
				t.Fatalf("discovered %+v, want the Codex app session", sessions)
			}
		})
	}
}
