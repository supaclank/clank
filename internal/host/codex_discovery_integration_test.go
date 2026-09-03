package host_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	sdk "github.com/coder/acp-go-sdk"
	"github.com/supaclank/clank/internal/agent"
	"github.com/supaclank/clank/internal/host/hosttest"
)

func TestIntegration_CodexACP_DiscoveryPaginates(t *testing.T) {
	const sessionCount = 205
	projectDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	otherDir := t.TempDir()
	sessions := make([]agent.SessionSnapshot, sessionCount)
	for i := range sessions {
		sessions[i] = agent.SessionSnapshot{
			ID:        fmt.Sprintf("00000000-0000-4000-8000-%012d", i),
			Title:     fmt.Sprintf("Codex discovery session %d", i),
			Directory: otherDir,
			UpdatedAt: time.Now().Add(-time.Duration(i) * time.Minute),
		}
	}
	target := &sessions[len(sessions)-1]
	target.Directory = projectDir
	mgr := hosttest.NewCodexDiscoveryManager(t, sessions)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	conn, err := mgr.Supervisor().GetConn(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	first, err := conn.Conn().ListSessions(ctx, sdk.ListSessionsRequest{Cwd: &projectDir})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Sessions) != 0 || first.NextCursor == nil {
		t.Fatalf("fixture must have an empty filtered first page with more pages: %+v", first)
	}
	for _, tc := range []struct {
		name     string
		isGlobal bool
	}{
		{name: "global", isGlobal: true},
		{name: "project", isGlobal: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []agent.SessionSnapshot
			var err error
			want := sessionCount
			if tc.isGlobal {
				got, err = mgr.DiscoverAllSessions(ctx)
			} else {
				got, err = mgr.DiscoverSessions(ctx, projectDir)
				want = 1
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != want {
				t.Fatalf("discovered %d sessions, want %d across every page", len(got), want)
			}
			seen := make(map[string]bool)
			for _, session := range got {
				if seen[session.ID] {
					t.Errorf("duplicate session %s", session.ID)
				}
				seen[session.ID] = true
				if session.Backend != agent.BackendCodex {
					t.Errorf("backend = %s, want Codex", session.Backend)
				}
			}
			if !seen[target.ID] {
				t.Errorf("session from the final page is missing: %s", target.ID)
			}
		})
	}
	t.Run("empty", func(t *testing.T) {
		got, err := mgr.DiscoverSessions(ctx, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if got == nil || len(got) != 0 {
			t.Fatalf("empty discovery = %+v, want a non-nil empty slice", got)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		canceled, stop := context.WithCancel(t.Context())
		stop()
		if _, err := mgr.DiscoverAllSessions(canceled); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled discovery: %v", err)
		}
	})
}
