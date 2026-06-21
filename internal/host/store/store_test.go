package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host/store"
)

func mustOpen(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "host.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpen_CreatesDB(t *testing.T) {
	t.Parallel()
	s := mustOpen(t)
	rows, err := s.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions on fresh DB: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("fresh DB returned %d sessions, want 0", len(rows))
	}
}

func TestOpen_MigrationIdempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "host.db")
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

func TestUpsertAndGetSession(t *testing.T) {
	t.Parallel()
	s := mustOpen(t)
	ctx := context.Background()

	now := time.Now().Truncate(time.Second)
	info := agent.SessionInfo{
		ID:         "ses-001",
		ExternalID: "oc-ext-001",
		Backend:    agent.BackendOpenCode,
		Status:     agent.StatusBusy,
		Visibility: agent.VisibilityDone,
		FollowUp:   true,
		GitRef:     agent.GitRef{LocalPath: "/tmp/repo", WorktreeID: "https://github.com/x/y", WorktreeBranch: "feat/login"},
		Prompt:     "Fix the login bug",
		Title:      "Fix authentication",
		TicketID:   "TICKET-42",
		Agent:      "plan",
		Draft:      "wip",
		CreatedAt:  now.Add(-2 * time.Hour),
		UpdatedAt:  now,
		LastReadAt: now,
	}

	if err := s.UpsertSession(ctx, info); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	got, err := s.GetSession(ctx, info.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.ExternalID != info.ExternalID || got.Backend != info.Backend || got.Status != info.Status {
		t.Errorf("round-trip mismatch on identity fields:\nwant %+v\n got %+v", info, got)
	}
	if got.Visibility != info.Visibility {
		t.Errorf("visibility: got %q, want %q", got.Visibility, info.Visibility)
	}
	if !got.FollowUp {
		t.Error("follow_up was lost")
	}
	if got.GitRef.LocalPath != info.GitRef.LocalPath || got.GitRef.WorktreeID != info.GitRef.WorktreeID || got.GitRef.WorktreeBranch != info.GitRef.WorktreeBranch {
		t.Errorf("gitref:\n got %+v\nwant %+v", got.GitRef, info.GitRef)
	}
	if !got.LastReadAt.Equal(now) {
		t.Errorf("last_read_at: got %v, want %v", got.LastReadAt, now)
	}
}

func TestUpsertSession_RequiresID(t *testing.T) {
	t.Parallel()
	s := mustOpen(t)
	if err := s.UpsertSession(context.Background(), agent.SessionInfo{}); err == nil {
		t.Error("UpsertSession with empty ID returned nil error")
	}
}

func TestGetSession_NotFound(t *testing.T) {
	t.Parallel()
	s := mustOpen(t)
	_, err := s.GetSession(context.Background(), "missing")
	if !errors.Is(err, store.ErrSessionNotFound) {
		t.Errorf("want ErrSessionNotFound, got %v", err)
	}
}

func TestFindSessionByExternalID(t *testing.T) {
	t.Parallel()
	s := mustOpen(t)
	ctx := context.Background()

	now := time.Now().Truncate(time.Second)
	info := agent.SessionInfo{
		ID:         "ses-001",
		ExternalID: "oc-ext-XYZ",
		Backend:    agent.BackendOpenCode,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := s.UpsertSession(ctx, info); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	got, err := s.FindSessionByExternalID(ctx, "oc-ext-XYZ")
	if err != nil {
		t.Fatalf("FindSessionByExternalID: %v", err)
	}
	if got.ID != "ses-001" {
		t.Errorf("found wrong session: got id %q", got.ID)
	}

	_, err = s.FindSessionByExternalID(ctx, "no-such-thing")
	if !errors.Is(err, store.ErrSessionNotFound) {
		t.Errorf("missing external_id: want ErrSessionNotFound, got %v", err)
	}
}

func TestSearchSessions(t *testing.T) {
	t.Parallel()
	s := mustOpen(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	mk := func(id, title, draft string, vis agent.SessionVisibility, ts time.Time) agent.SessionInfo {
		return agent.SessionInfo{
			ID:         id,
			Backend:    agent.BackendOpenCode,
			Title:      title,
			Draft:      draft,
			Visibility: vis,
			CreatedAt:  ts,
			UpdatedAt:  ts,
		}
	}
	for _, info := range []agent.SessionInfo{
		mk("a", "Login refactor", "", "", now.Add(-30*time.Minute)),
		mk("b", "Routing tweaks", "", agent.VisibilityDone, now.Add(-20*time.Minute)),
		mk("c", "Misc", "draft-text", "", now.Add(-10*time.Minute)),
		mk("d", "Logging", "", agent.VisibilityArchived, now.Add(-5*time.Minute)),
	} {
		if err := s.UpsertSession(ctx, info); err != nil {
			t.Fatalf("UpsertSession %s: %v", info.ID, err)
		}
	}

	all, err := s.SearchSessions(ctx, store.SearchParams{})
	if err != nil {
		t.Fatalf("SearchSessions all: %v", err)
	}
	if len(all) != 4 {
		t.Errorf("all: got %d, want 4", len(all))
	}

	// Visibility filter
	done, err := s.SearchSessions(ctx, store.SearchParams{Visibility: agent.VisibilityDone})
	if err != nil {
		t.Fatalf("SearchSessions visibility=done: %v", err)
	}
	if len(done) != 1 || done[0].ID != "b" {
		t.Errorf("visibility=done: got %v", done)
	}

	// Query filter (title match)
	q, err := s.SearchSessions(ctx, store.SearchParams{Q: "Log"})
	if err != nil {
		t.Fatalf("SearchSessions q=Log: %v", err)
	}
	// Should match "Login refactor" + "Logging" but NOT "draft-text" / "Misc" / "Routing tweaks".
	wantIDs := map[string]bool{"a": true, "d": true}
	for _, info := range q {
		if !wantIDs[info.ID] {
			t.Errorf("q=Log returned unexpected id %q (title=%q)", info.ID, info.Title)
		}
		delete(wantIDs, info.ID)
	}
	if len(wantIDs) != 0 {
		t.Errorf("q=Log missed expected ids: %v", wantIDs)
	}

	// Query against draft text
	d, err := s.SearchSessions(ctx, store.SearchParams{Q: "draft"})
	if err != nil {
		t.Fatalf("SearchSessions q=draft: %v", err)
	}
	if len(d) != 1 || d[0].ID != "c" {
		t.Errorf("q=draft: got %v", d)
	}
}

func TestDeleteSession(t *testing.T) {
	t.Parallel()
	s := mustOpen(t)
	ctx := context.Background()
	now := time.Now()
	info := agent.SessionInfo{ID: "x", Backend: agent.BackendOpenCode, CreatedAt: now, UpdatedAt: now}

	if err := s.UpsertSession(ctx, info); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(ctx, "x"); !errors.Is(err, store.ErrSessionNotFound) {
		t.Errorf("after delete: got %v, want ErrSessionNotFound", err)
	}
	// Delete on missing row is a no-op.
	if err := s.DeleteSession(ctx, "x"); err != nil {
		t.Errorf("delete missing: %v", err)
	}
}

func TestPrimaryAgents_RoundTrip(t *testing.T) {
	t.Parallel()
	s := mustOpen(t)
	ctx := context.Background()

	ref := agent.GitRef{LocalPath: "/repo", WorktreeID: "https://github.com/x/y"}
	want := []agent.AgentInfo{
		{Name: "plan", Description: "Plan", Mode: "primary"},
		{Name: "code", Description: "Code", Mode: "primary"},
	}
	if err := s.UpsertPrimaryAgents(ctx, agent.BackendOpenCode, ref, want); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := s.LoadPrimaryAgents(ctx, agent.BackendOpenCode, ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != len(want) || got[0].Name != "plan" || got[1].Name != "code" {
		t.Errorf("round-trip: got %v, want %v", got, want)
	}

	// Missing entry → nil result, nil error.
	missing, err := s.LoadPrimaryAgents(ctx, agent.BackendClaudeCode, ref)
	if err != nil {
		t.Errorf("missing: %v", err)
	}
	if missing != nil {
		t.Errorf("missing: got %v, want nil", missing)
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "host.db")

	{
		s, err := store.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		_ = s.UpsertSession(context.Background(), agent.SessionInfo{
			ID: "p", Backend: agent.BackendOpenCode, CreatedAt: now, UpdatedAt: now,
		})
		s.Close()
	}

	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	got, err := s.GetSession(context.Background(), "p")
	if err != nil {
		t.Fatalf("after reopen: %v", err)
	}
	if got.ID != "p" {
		t.Errorf("after reopen: got %+v", got)
	}
}

// TestOpen_MigrationV1ToV2_RenamesGitRemoteToWorktreeID exercises the
// in-place column rename for dev installs whose host.db was created
// when sessions/primary_agents had a `git_remote` column. After
// re-Open, queries against the new `worktree_id` column must work.
func TestOpen_MigrationV1ToV2_RenamesGitRemoteToWorktreeID(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "host.db")

	// Hand-craft a v1 database with the legacy git_remote column.
	rawDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB.Exec(`
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			external_id TEXT NOT NULL DEFAULT '',
			backend TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'idle',
			visibility TEXT NOT NULL DEFAULT '',
			follow_up INTEGER NOT NULL DEFAULT 0,
			project_dir TEXT NOT NULL DEFAULT '',
			git_remote TEXT NOT NULL DEFAULT '',
			worktree_branch TEXT NOT NULL DEFAULT '',
			prompt TEXT NOT NULL DEFAULT '',
			title TEXT NOT NULL DEFAULT '',
			ticket_id TEXT NOT NULL DEFAULT '',
			agent TEXT NOT NULL DEFAULT '',
			draft TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			last_read_at DATETIME
		);
		CREATE TABLE primary_agents (
			backend TEXT NOT NULL,
			project_dir TEXT NOT NULL DEFAULT '',
			git_remote TEXT NOT NULL DEFAULT '',
			primary_agents_json TEXT NOT NULL DEFAULT '[]',
			updated_at DATETIME NOT NULL,
			PRIMARY KEY (backend, project_dir, git_remote)
		);
		INSERT INTO sessions(id, backend, git_remote, created_at, updated_at)
		    VALUES ('legacy-1', 'opencode', 'git@github.com:x/y.git', '2026-01-01', '2026-01-01');
		PRAGMA user_version = 1;
	`); err != nil {
		rawDB.Close()
		t.Fatalf("seed v1 DB: %v", err)
	}
	rawDB.Close()

	// Re-open with the new code; v2 migration should rename the
	// column AND clear the URL-shaped value (we want a fresh slate
	// since the column's meaning changed, not just its name).
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("re-open after v2: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	got, err := s.GetSession(context.Background(), "legacy-1")
	if err != nil {
		t.Fatalf("GetSession after migration: %v", err)
	}
	if got.GitRef.WorktreeID != "" {
		t.Errorf("expected legacy git URL cleared, got %q", got.GitRef.WorktreeID)
	}
}

// TestUpsertSession_TimeNowRoundTripsLosslessly is the regression
// test for the modernc.org/sqlite monotonic-clock bug: a time.Now()
// inserted via UpsertSession used to be stored as Go's String()
// output (including the m=+... suffix), and ListSessions then
// failed with "Scan error … unsupported Scan, storing driver.Value
// type string into type *time.Time". After the v3 INTEGER-millis
// migration the bug is structurally impossible — this test pins
// that property so a future schema change can't silently
// re-introduce it.
func TestUpsertSession_TimeNowRoundTripsLosslessly(t *testing.T) {
	t.Parallel()
	s := mustOpen(t)
	ctx := context.Background()

	now := time.Now()
	if err := s.UpsertSession(ctx, agent.SessionInfo{
		ID:        "roundtrip",
		Backend:   agent.BackendOpenCode,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	got, err := s.GetSession(ctx, "roundtrip")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.CreatedAt.UnixMilli() != now.UnixMilli() {
		t.Errorf("CreatedAt drift: got %d, want %d (delta %d ms)",
			got.CreatedAt.UnixMilli(), now.UnixMilli(),
			got.CreatedAt.UnixMilli()-now.UnixMilli())
	}
	// And the part that used to be impossible: ListSessions must
	// scan every row without driver-level errors.
	all, err := s.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v (this is the modernc bug we fixed)", err)
	}
	if len(all) != 1 {
		t.Errorf("ListSessions returned %d rows, want 1", len(all))
	}
}

// TestOpen_MigrationV2ToV3_BackfillsTimeFormats hand-crafts a v2
// database carrying every stringified-time format we observed on
// production sprites, runs the v3 migration, and asserts every row
// survived with a sensible millis value. The rows used to be
// unreadable post-migration if the parser missed one of the formats.
func TestOpen_MigrationV2ToV3_BackfillsTimeFormats(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "host.db")

	rawDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB.Exec(`
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			external_id TEXT NOT NULL DEFAULT '',
			backend TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'idle',
			visibility TEXT NOT NULL DEFAULT '',
			follow_up INTEGER NOT NULL DEFAULT 0,
			project_dir TEXT NOT NULL DEFAULT '',
			worktree_id TEXT NOT NULL DEFAULT '',
			worktree_branch TEXT NOT NULL DEFAULT '',
			prompt TEXT NOT NULL DEFAULT '',
			title TEXT NOT NULL DEFAULT '',
			ticket_id TEXT NOT NULL DEFAULT '',
			agent TEXT NOT NULL DEFAULT '',
			draft TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			last_read_at DATETIME
		);
		CREATE TABLE primary_agents (
			backend TEXT NOT NULL,
			project_dir TEXT NOT NULL DEFAULT '',
			worktree_id TEXT NOT NULL DEFAULT '',
			primary_agents_json TEXT NOT NULL DEFAULT '[]',
			updated_at DATETIME NOT NULL,
			PRIMARY KEY (backend, project_dir, worktree_id)
		);
		PRAGMA user_version = 2;
	`); err != nil {
		rawDB.Close()
		t.Fatalf("seed v2 DB: %v", err)
	}

	// Seed one row per legacy format. The expected post-migration
	// millis is the same time encoded six different ways (we use
	// minute precision so the fixed-offset row's TZ math works out
	// to the same instant as the UTC rows).
	for _, c := range []struct{ id, raw string }{
		{"monotonic-plus", "2026-05-12 14:55:00 +0000 UTC m=+1046.173573732"},
		{"monotonic-minus", "2026-05-12 14:55:00 +0000 UTC m=-12.345"},
		{"utc-named", "2026-05-12 14:55:00 +0000 UTC"},
		{"fixed-offset", "2026-05-12 16:55:00 +0200 +0200"},
		{"rfc3339nano", "2026-05-12T14:55:00.000000000Z"},
		{"rfc3339", "2026-05-12T14:55:00Z"},
	} {
		if _, err := rawDB.Exec(
			`INSERT INTO sessions(id, backend, created_at, updated_at) VALUES (?, 'opencode', ?, ?)`,
			c.id, c.raw, c.raw,
		); err != nil {
			rawDB.Close()
			t.Fatalf("seed %s: %v", c.id, err)
		}
	}
	rawDB.Close()

	// Re-open: v3 migration must run, every row must survive.
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("re-open after v3: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	wantMs := time.Date(2026, 5, 12, 14, 55, 0, 0, time.UTC).UnixMilli()
	all, err := s.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions after migration: %v", err)
	}
	if len(all) != 6 {
		t.Fatalf("expected 6 sessions after v3, got %d", len(all))
	}
	for _, row := range all {
		if row.CreatedAt.UnixMilli() != wantMs {
			t.Errorf("row %s: CreatedAt = %d ms (%s), want %d ms",
				row.ID, row.CreatedAt.UnixMilli(), row.CreatedAt.Format(time.RFC3339Nano), wantMs)
		}
	}
}

// TestOpen_MigrationV3_IdempotentOnReentry simulates the partial-failure
// case CodeRabbit flagged: migrateSessionsToMillis commits, but the
// outer migrate() exits before bumping user_version to 3 (e.g. helper #2
// panics, process crashes, etc.). On next Open the v3 block re-runs;
// sessions is now INTEGER, so CAST(created_at AS TEXT) produces digit
// strings like "1716000000000". Pre-fix, parseLegacyTimeMs didn't
// recognise that shape and fell back to ULID/now(), silently rewriting
// the timestamps. After the fix it passes the integer through unchanged.
func TestOpen_MigrationV3_IdempotentOnReentry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "host.db")

	// Open once → full v3 migration runs from a fresh DB.
	s1, err := store.Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	now := time.Now()
	if err := s1.UpsertSession(context.Background(), agent.SessionInfo{
		ID:        "01KREAZB05YBB9P6QVH0SRZC1F",
		Backend:   agent.BackendOpenCode,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	wantCreatedMs := now.UnixMilli()
	s1.Close()

	// Hand-rewind user_version to 2 so the next Open re-runs v3 on a
	// table whose schema is already INTEGER. This is the
	// half-committed shape the CR comment is about.
	rawDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := rawDB.Exec(`PRAGMA user_version = 2`); err != nil {
		rawDB.Close()
		t.Fatalf("rewind user_version: %v", err)
	}
	rawDB.Close()

	// Re-open. v3 reruns; parseLegacyTimeMs must recognise the digit
	// strings produced by CAST(INTEGER AS TEXT) and pass them through.
	s2, err := store.Open(path)
	if err != nil {
		t.Fatalf("re-open after rewind: %v", err)
	}
	t.Cleanup(func() { s2.Close() })

	got, err := s2.GetSession(context.Background(), "01KREAZB05YBB9P6QVH0SRZC1F")
	if err != nil {
		t.Fatalf("GetSession after re-entry: %v", err)
	}
	if got.CreatedAt.UnixMilli() != wantCreatedMs {
		t.Errorf("timestamp rewritten by re-entry: got %d ms, want %d ms (delta %d)",
			got.CreatedAt.UnixMilli(), wantCreatedMs,
			got.CreatedAt.UnixMilli()-wantCreatedMs)
	}
}

// TestOpen_MigrationV2ToV3_UnparseableTimeFallsBackToULID covers the
// last-ditch path of the v3 migration: if a row's stored time is
// completely unparseable (corruption, missing format, etc.) the
// migration uses the row's ULID-encoded timestamp instead of
// failing. Crucial property — one bad row must not block startup.
func TestOpen_MigrationV2ToV3_UnparseableTimeFallsBackToULID(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "host.db")

	rawDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// Seed a minimal v2 schema with one row whose timestamps are
	// nonsense.
	if _, err := rawDB.Exec(`
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY, external_id TEXT NOT NULL DEFAULT '',
			backend TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'idle',
			visibility TEXT NOT NULL DEFAULT '', follow_up INTEGER NOT NULL DEFAULT 0,
			project_dir TEXT NOT NULL DEFAULT '', worktree_id TEXT NOT NULL DEFAULT '',
			worktree_branch TEXT NOT NULL DEFAULT '', prompt TEXT NOT NULL DEFAULT '',
			title TEXT NOT NULL DEFAULT '', ticket_id TEXT NOT NULL DEFAULT '',
			agent TEXT NOT NULL DEFAULT '', draft TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL,
			last_read_at DATETIME
		);
		CREATE TABLE primary_agents (
			backend TEXT NOT NULL, project_dir TEXT NOT NULL DEFAULT '',
			worktree_id TEXT NOT NULL DEFAULT '', primary_agents_json TEXT NOT NULL DEFAULT '[]',
			updated_at DATETIME NOT NULL,
			PRIMARY KEY (backend, project_dir, worktree_id)
		);
		INSERT INTO sessions(id, backend, created_at, updated_at)
		    VALUES ('01KREAZB05YBB9P6QVH0SRZC1F', 'opencode', 'utter garbage', 'utter garbage');
		PRAGMA user_version = 2;
	`); err != nil {
		rawDB.Close()
		t.Fatalf("seed v2: %v", err)
	}
	rawDB.Close()

	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("re-open: v3 migration must not fail on a single bad row: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	got, err := s.GetSession(context.Background(), "01KREAZB05YBB9P6QVH0SRZC1F")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	// ULID-derived fallback should produce a non-zero, sensible time.
	if got.CreatedAt.IsZero() {
		t.Errorf("CreatedAt was zero — fallback path didn't fire")
	}
	if got.CreatedAt.Year() < 2024 {
		t.Errorf("CreatedAt %v looks pre-2024; ULID-decode probably failed", got.CreatedAt)
	}
}
