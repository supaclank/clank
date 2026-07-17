package daemoncli

// End-to-end coverage for monorepo-subdir GitRefs: the daemon accepts a
// local_path pointing inside a repo, sessions normalize to
// {root, subdir}, and the preview no_preview code survives
// CLI → gateway → host so only genuine "no app here" errors carry the
// Expo/Vite hint.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/daemonclient"
	"github.com/acksell/clank/internal/host"
	"github.com/acksell/clank/internal/host/hosttest"
)

// TestPreviewStart_SubdirSlug_NoPreviewSurvivesGateway proves the
// structured no_preview code crosses the whole stack intact: for a
// folder-slug key naming a repo subdir with no detectable app, the
// daemonclient surfaces ErrNotPreviewable — previously this path 500'd
// with a misleading "local_path is not the repo root".
func TestPreviewStart_SubdirSlug_NoPreviewSurvivesGateway(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)

	repo := hosttest.InitGitRepo(t)
	sub := filepath.Join(repo, "web-app")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := td.Client.Preview(host.LocalRepoSlug(sub)).Start(ctx)
	if !errors.Is(err, daemonclient.ErrNotPreviewable) {
		t.Fatalf("want ErrNotPreviewable, got %v", err)
	}
}

// TestCreateSession_Subdir_NormalizedThroughGateway pins the wire
// contract end to end: creating a session from a monorepo subdir
// returns AND persists {LocalPath: root, Subdir: rel} with the folder
// name as DisplayName, so clients key on the repo while the session
// runs in the folder the user started in.
func TestCreateSession_Subdir_NormalizedThroughGateway(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)

	repo := hosttest.InitGitRepo(t)
	sub := filepath.Join(repo, "web-app")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := td.Client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{LocalPath: sub},
		Prompt:  "hi",
	})
	if err != nil {
		t.Fatalf("Sessions().Create: %v", err)
	}

	want := agent.GitRef{LocalPath: repo, Subdir: "web-app", DisplayName: "web-app"}
	if info.GitRef != want {
		t.Errorf("response ref = %+v, want %+v", info.GitRef, want)
	}
	row, err := td.Store.GetSession(ctx, info.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if row.GitRef != want {
		t.Errorf("persisted ref = %+v, want %+v", row.GitRef, want)
	}
}
