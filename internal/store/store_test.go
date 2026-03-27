package store_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/store"
)

func tempDBPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return filepath.Join(dir, "test.db")
}

func mustOpen(t *testing.T, path string) *store.Store {
	t.Helper()
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenCreatesDB(t *testing.T) {
	t.Parallel()
	path := tempDBPath(t)

	// File should not exist yet.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("expected DB file to not exist before Open")
	}

	s := mustOpen(t, path)

	// File should now exist.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected DB file to exist after Open: %v", err)
	}

	// Should be able to load sessions (empty).
	sessions, err := s.LoadSessions()
	if err != nil {
		t.Fatalf("LoadSessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions from fresh DB, got %d", len(sessions))
	}
}

func TestMigrationIdempotent(t *testing.T) {
	t.Parallel()
	path := tempDBPath(t)

	// Open twice — migration should be idempotent.
	s1, err := store.Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	s1.Close()

	s2, err := store.Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	s2.Close()
}

func TestUpsertAndLoad(t *testing.T) {
	t.Parallel()
	s := mustOpen(t, tempDBPath(t))

	now := time.Now().Truncate(time.Millisecond)
	info := agent.SessionInfo{
		ID:          "ses-001",
		ExternalID:  "oc-ext-001",
		Backend:     agent.BackendOpenCode,
		Status:      agent.StatusBusy,
		Visibility:  agent.VisibilityDone,
		FollowUp:    true,
		ProjectDir:  "/tmp/project-a",
		ProjectName: "project-a",
		Prompt:      "Fix the login bug",
		Title:       "Fix authentication",
		TicketID:    "TICKET-42",
		Agent:       "plan",
		Draft:       "work in progress",
		CreatedAt:   now.Add(-2 * time.Hour),
		UpdatedAt:   now.Add(-1 * time.Hour),
		LastReadAt:  now,
	}

	if err := s.UpsertSession(info); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	sessions, err := s.LoadSessions()
	if err != nil {
		t.Fatalf("LoadSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}

	got := sessions[0]
	if got.ID != info.ID {
		t.Errorf("ID = %q, want %q", got.ID, info.ID)
	}
	if got.ExternalID != info.ExternalID {
		t.Errorf("ExternalID = %q, want %q", got.ExternalID, info.ExternalID)
	}
	if got.Backend != info.Backend {
		t.Errorf("Backend = %q, want %q", got.Backend, info.Backend)
	}
	if got.Status != info.Status {
		t.Errorf("Status = %q, want %q", got.Status, info.Status)
	}
	if got.Visibility != info.Visibility {
		t.Errorf("Visibility = %q, want %q", got.Visibility, info.Visibility)
	}
	if got.FollowUp != info.FollowUp {
		t.Errorf("FollowUp = %v, want %v", got.FollowUp, info.FollowUp)
	}
	if got.ProjectDir != info.ProjectDir {
		t.Errorf("ProjectDir = %q, want %q", got.ProjectDir, info.ProjectDir)
	}
	if got.ProjectName != info.ProjectName {
		t.Errorf("ProjectName = %q, want %q", got.ProjectName, info.ProjectName)
	}
	if got.Prompt != info.Prompt {
		t.Errorf("Prompt = %q, want %q", got.Prompt, info.Prompt)
	}
	if got.Title != info.Title {
		t.Errorf("Title = %q, want %q", got.Title, info.Title)
	}
	if got.TicketID != info.TicketID {
		t.Errorf("TicketID = %q, want %q", got.TicketID, info.TicketID)
	}
	if got.Agent != info.Agent {
		t.Errorf("Agent = %q, want %q", got.Agent, info.Agent)
	}
	if got.Draft != info.Draft {
		t.Errorf("Draft = %q, want %q", got.Draft, info.Draft)
	}
	if !got.CreatedAt.Equal(info.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, info.CreatedAt)
	}
	if !got.UpdatedAt.Equal(info.UpdatedAt) {
		t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, info.UpdatedAt)
	}
	if !got.LastReadAt.Equal(info.LastReadAt) {
		t.Errorf("LastReadAt = %v, want %v", got.LastReadAt, info.LastReadAt)
	}
}

func TestUpsertUpdatesExisting(t *testing.T) {
	t.Parallel()
	s := mustOpen(t, tempDBPath(t))

	now := time.Now().Truncate(time.Millisecond)
	info := agent.SessionInfo{
		ID:          "ses-002",
		Backend:     agent.BackendOpenCode,
		Status:      agent.StatusIdle,
		ProjectDir:  "/tmp/project-b",
		ProjectName: "project-b",
		Prompt:      "original prompt",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.UpsertSession(info); err != nil {
		t.Fatalf("UpsertSession (initial): %v", err)
	}

	// Update fields.
	info.Visibility = agent.VisibilityArchived
	info.FollowUp = true
	info.Title = "Updated title"
	info.Draft = "new draft"
	info.UpdatedAt = now.Add(1 * time.Hour)
	if err := s.UpsertSession(info); err != nil {
		t.Fatalf("UpsertSession (update): %v", err)
	}

	sessions, err := s.LoadSessions()
	if err != nil {
		t.Fatalf("LoadSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session after upsert, got %d", len(sessions))
	}

	got := sessions[0]
	if got.Visibility != agent.VisibilityArchived {
		t.Errorf("Visibility = %q, want %q", got.Visibility, agent.VisibilityArchived)
	}
	if !got.FollowUp {
		t.Error("expected FollowUp = true")
	}
	if got.Title != "Updated title" {
		t.Errorf("Title = %q, want %q", got.Title, "Updated title")
	}
	if got.Draft != "new draft" {
		t.Errorf("Draft = %q, want %q", got.Draft, "new draft")
	}
}

func TestDeleteSession(t *testing.T) {
	t.Parallel()
	s := mustOpen(t, tempDBPath(t))

	now := time.Now().Truncate(time.Millisecond)
	info := agent.SessionInfo{
		ID:          "ses-003",
		Backend:     agent.BackendOpenCode,
		Status:      agent.StatusIdle,
		ProjectDir:  "/tmp/project-c",
		ProjectName: "project-c",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.UpsertSession(info); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	if err := s.DeleteSession("ses-003"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	sessions, err := s.LoadSessions()
	if err != nil {
		t.Fatalf("LoadSessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions after delete, got %d", len(sessions))
	}
}

func TestDeleteNonexistentSession(t *testing.T) {
	t.Parallel()
	s := mustOpen(t, tempDBPath(t))

	// Deleting a nonexistent session should not error.
	if err := s.DeleteSession("nonexistent"); err != nil {
		t.Fatalf("DeleteSession(nonexistent): %v", err)
	}
}

func TestLoadSessionsEmpty(t *testing.T) {
	t.Parallel()
	s := mustOpen(t, tempDBPath(t))

	sessions, err := s.LoadSessions()
	if err != nil {
		t.Fatalf("LoadSessions: %v", err)
	}
	if sessions == nil {
		t.Error("expected non-nil empty slice, got nil")
	}
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(sessions))
	}
}

func TestLastReadAtZeroValue(t *testing.T) {
	t.Parallel()
	s := mustOpen(t, tempDBPath(t))

	now := time.Now().Truncate(time.Millisecond)
	info := agent.SessionInfo{
		ID:          "ses-004",
		Backend:     agent.BackendOpenCode,
		Status:      agent.StatusIdle,
		ProjectDir:  "/tmp/project-d",
		ProjectName: "project-d",
		CreatedAt:   now,
		UpdatedAt:   now,
		// LastReadAt intentionally left as zero value.
	}
	if err := s.UpsertSession(info); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	sessions, err := s.LoadSessions()
	if err != nil {
		t.Fatalf("LoadSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if !sessions[0].LastReadAt.IsZero() {
		t.Errorf("expected zero LastReadAt, got %v", sessions[0].LastReadAt)
	}
}

func TestFindByExternalID(t *testing.T) {
	t.Parallel()
	s := mustOpen(t, tempDBPath(t))

	now := time.Now().Truncate(time.Millisecond)
	info := agent.SessionInfo{
		ID:          "ses-005",
		ExternalID:  "oc-ext-005",
		Backend:     agent.BackendOpenCode,
		Status:      agent.StatusIdle,
		Visibility:  agent.VisibilityDone,
		FollowUp:    true,
		ProjectDir:  "/tmp/project-e",
		ProjectName: "project-e",
		Draft:       "saved draft",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.UpsertSession(info); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	// Found case.
	got, err := s.FindByExternalID("oc-ext-005")
	if err != nil {
		t.Fatalf("FindByExternalID: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	if got.ID != "ses-005" {
		t.Errorf("ID = %q, want %q", got.ID, "ses-005")
	}
	if got.Visibility != agent.VisibilityDone {
		t.Errorf("Visibility = %q, want %q", got.Visibility, agent.VisibilityDone)
	}
	if !got.FollowUp {
		t.Error("expected FollowUp = true")
	}
	if got.Draft != "saved draft" {
		t.Errorf("Draft = %q, want %q", got.Draft, "saved draft")
	}

	// Not found case.
	got, err = s.FindByExternalID("nonexistent")
	if err != nil {
		t.Fatalf("FindByExternalID(nonexistent): %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for nonexistent external_id, got %+v", got)
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	t.Parallel()
	path := tempDBPath(t)

	now := time.Now().Truncate(time.Millisecond)
	info := agent.SessionInfo{
		ID:          "ses-006",
		ExternalID:  "oc-ext-006",
		Backend:     agent.BackendOpenCode,
		Status:      agent.StatusBusy,
		Visibility:  agent.VisibilityDone,
		FollowUp:    true,
		ProjectDir:  "/tmp/project-f",
		ProjectName: "project-f",
		Prompt:      "do stuff",
		Title:       "Doing stuff",
		Draft:       "my draft",
		CreatedAt:   now.Add(-1 * time.Hour),
		UpdatedAt:   now,
		LastReadAt:  now,
	}

	// Write with first store instance.
	s1, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open (1): %v", err)
	}
	if err := s1.UpsertSession(info); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	s1.Close()

	// Read with second store instance.
	s2, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open (2): %v", err)
	}
	defer s2.Close()

	sessions, err := s2.LoadSessions()
	if err != nil {
		t.Fatalf("LoadSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}

	got := sessions[0]
	if got.ID != "ses-006" {
		t.Errorf("ID = %q, want %q", got.ID, "ses-006")
	}
	if got.Visibility != agent.VisibilityDone {
		t.Errorf("Visibility = %q, want %q", got.Visibility, agent.VisibilityDone)
	}
	if !got.FollowUp {
		t.Error("expected FollowUp = true")
	}
	if got.Draft != "my draft" {
		t.Errorf("Draft = %q, want %q", got.Draft, "my draft")
	}
	if got.Status != agent.StatusBusy {
		t.Errorf("Status = %q, want %q", got.Status, agent.StatusBusy)
	}
}

func TestMultipleSessions(t *testing.T) {
	t.Parallel()
	s := mustOpen(t, tempDBPath(t))

	now := time.Now().Truncate(time.Millisecond)
	for i, id := range []string{"ses-a", "ses-b", "ses-c"} {
		info := agent.SessionInfo{
			ID:          id,
			Backend:     agent.BackendOpenCode,
			Status:      agent.StatusIdle,
			ProjectDir:  "/tmp/project",
			ProjectName: "project",
			CreatedAt:   now.Add(time.Duration(i) * time.Minute),
			UpdatedAt:   now.Add(time.Duration(i) * time.Minute),
		}
		if err := s.UpsertSession(info); err != nil {
			t.Fatalf("UpsertSession(%s): %v", id, err)
		}
	}

	sessions, err := s.LoadSessions()
	if err != nil {
		t.Fatalf("LoadSessions: %v", err)
	}
	if len(sessions) != 3 {
		t.Fatalf("expected 3 sessions, got %d", len(sessions))
	}

	// Delete the middle one.
	if err := s.DeleteSession("ses-b"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	sessions, err = s.LoadSessions()
	if err != nil {
		t.Fatalf("LoadSessions after delete: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}
	ids := map[string]bool{}
	for _, s := range sessions {
		ids[s.ID] = true
	}
	if ids["ses-b"] {
		t.Error("expected ses-b to be deleted")
	}
	if !ids["ses-a"] || !ids["ses-c"] {
		t.Errorf("expected ses-a and ses-c to remain, got %v", ids)
	}
}
