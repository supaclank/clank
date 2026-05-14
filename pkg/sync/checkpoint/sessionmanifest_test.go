package checkpoint_test

import (
	"strings"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/pkg/sync/checkpoint"
)

func TestSessionManifest_RoundTripJSON(t *testing.T) {
	t.Parallel()
	want := &checkpoint.SessionManifest{
		Version:      checkpoint.SessionManifestVersion,
		CheckpointID: "ck-1",
		CreatedAt:    time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
		CreatedBy:    "laptop:dev",
		Sessions: []checkpoint.SessionEntry{
			{
				SessionID:      "01HX0001",
				ExternalID:     "ses_abc",
				Backend:        agent.BackendOpenCode,
				BlobKey:        "sessions/01HX0001.json",
				Status:         agent.StatusIdle,
				Title:          "Refactor auth",
				Prompt:         "Refactor the auth middleware",
				TicketID:       "JIRA-123",
				Agent:          "build",
				WorktreeBranch: "feature/auth",
				ProjectDir:     "/repo",
				CreatedAt:      time.Date(2026, 5, 13, 11, 0, 0, 0, time.UTC),
				UpdatedAt:      time.Date(2026, 5, 13, 11, 59, 0, 0, time.UTC),
				Bytes:          2048,
				WasBusy:        true,
			},
		},
	}
	data, err := want.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := checkpoint.UnmarshalSessionManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.CheckpointID != want.CheckpointID ||
		got.CreatedBy != want.CreatedBy ||
		!got.CreatedAt.Equal(want.CreatedAt) ||
		len(got.Sessions) != 1 {
		t.Fatalf("manifest round-trip mismatch:\n want %+v\n got  %+v", want, got)
	}
	gs, ws := got.Sessions[0], want.Sessions[0]
	if gs.SessionID != ws.SessionID ||
		gs.ExternalID != ws.ExternalID ||
		gs.Backend != ws.Backend ||
		gs.BlobKey != ws.BlobKey ||
		gs.Status != ws.Status ||
		gs.Title != ws.Title ||
		gs.Prompt != ws.Prompt ||
		gs.TicketID != ws.TicketID ||
		gs.Agent != ws.Agent ||
		gs.WorktreeBranch != ws.WorktreeBranch ||
		gs.ProjectDir != ws.ProjectDir ||
		!gs.CreatedAt.Equal(ws.CreatedAt) ||
		!gs.UpdatedAt.Equal(ws.UpdatedAt) ||
		gs.Bytes != ws.Bytes ||
		gs.WasBusy != ws.WasBusy {
		t.Fatalf("SessionEntry round-trip mismatch:\n want %+v\n got  %+v", ws, gs)
	}
}

func TestSessionManifest_RejectsUnknownVersion(t *testing.T) {
	t.Parallel()
	bogus := []byte(`{"version":999,"checkpoint_id":"ck-1","sessions":[]}`)
	_, err := checkpoint.UnmarshalSessionManifest(bogus)
	if err == nil {
		t.Fatal("expected error for unknown version, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported session manifest version 999") {
		t.Errorf("expected version-rejection error, got: %v", err)
	}
}

func TestSessionManifest_RejectsMalformedJSON(t *testing.T) {
	t.Parallel()
	_, err := checkpoint.UnmarshalSessionManifest([]byte("not json"))
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
}

func TestSessionManifest_EmptySessions(t *testing.T) {
	t.Parallel()
	m := &checkpoint.SessionManifest{
		Version:      checkpoint.SessionManifestVersion,
		CheckpointID: "ck-empty",
		CreatedAt:    time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
		CreatedBy:    "laptop:dev",
	}
	data, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := checkpoint.UnmarshalSessionManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Sessions) != 0 {
		t.Fatalf("want 0 sessions, got %d", len(got.Sessions))
	}
}
