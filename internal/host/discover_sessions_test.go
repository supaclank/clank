package host_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
)

// fakeProject mirrors opencode-sdk-go's Project shape just enough for List.
type fakeProject struct {
	ID       string          `json:"id"`
	Worktree string          `json:"worktree"`
	Time     fakeProjectTime `json:"time"`
}

type fakeProjectTime struct {
	Created float64 `json:"created"`
}

// fakeSession mirrors the subset of opencode-sdk-go's Session used by
// ListSessionsFromServer.
type fakeSession struct {
	ID        string          `json:"id"`
	Directory string          `json:"directory"`
	ProjectID string          `json:"projectID"`
	Title     string          `json:"title"`
	Version   string          `json:"version"`
	ParentID  string          `json:"parentID"`
	Time      fakeSessionTime `json:"time"`
	Revert    fakeRevert      `json:"revert"`
}

type fakeSessionTime struct {
	Created float64 `json:"created"`
	Updated float64 `json:"updated"`
}

type fakeRevert struct {
	MessageID string `json:"messageID"`
}

// fakeOpencodeServer simulates an opencode HTTP server scoped to a single
// project: /project lists ALL known projects (the global view), but /session
// returns only sessions belonging to this server's project.
type fakeOpencodeServer struct {
	t               *testing.T
	projectDir      string
	allProjects     []fakeProject
	mySessions      []fakeSession
	sessionListHits atomic.Int32
	lastLimit       atomic.Value // string
}

func (f *fakeOpencodeServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/global/health":
			w.WriteHeader(http.StatusOK)
		case "/project":
			writeJSON(w, f.allProjects)
		case "/session":
			f.sessionListHits.Add(1)
			f.lastLimit.Store(r.URL.Query().Get("limit"))
			writeJSON(w, f.mySessions)
		default:
			http.NotFound(w, r)
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// TestDiscoverSessionsAcrossAllProjects is a regression test for the bug
// where DiscoverSessions only queried /session on the seed server, which
// silently dropped every session belonging to other projects. opencode's
// HTTP API is project-scoped (each `opencode serve` returns only sessions
// for its own startup directory), even though the underlying SQLite DB is
// global. The fix iterates per-project, calling GetOrStartServer for each
// known project worktree.
func TestDiscoverSessionsAcrossAllProjects(t *testing.T) {
	t.Parallel()

	dirA := t.TempDir()
	dirB := t.TempDir()
	dirC := t.TempDir()

	allProjects := []fakeProject{
		{ID: "p-a", Worktree: dirA, Time: fakeProjectTime{Created: 1000}},
		{ID: "p-b", Worktree: dirB, Time: fakeProjectTime{Created: 2000}},
		{ID: "p-c", Worktree: dirC, Time: fakeProjectTime{Created: 3000}},
	}

	mkSession := func(id, dir, projID string) fakeSession {
		return fakeSession{
			ID: id, Directory: dir, ProjectID: projID,
			Title:   "title-" + id,
			Version: "v1",
			Time:    fakeSessionTime{Created: 1, Updated: 2},
		}
	}

	fakes := map[string]*fakeOpencodeServer{
		dirA: {t: t, projectDir: dirA, allProjects: allProjects, mySessions: []fakeSession{
			mkSession("ses-a1", dirA, "p-a"),
			mkSession("ses-a2", dirA, "p-a"),
		}},
		dirB: {t: t, projectDir: dirB, allProjects: allProjects, mySessions: []fakeSession{
			mkSession("ses-b1", dirB, "p-b"),
		}},
		dirC: {t: t, projectDir: dirC, allProjects: allProjects, mySessions: []fakeSession{
			mkSession("ses-c1", dirC, "p-c"),
			mkSession("ses-c2", dirC, "p-c"),
			mkSession("ses-c3", dirC, "p-c"),
			// A child/forked session — must be filtered out.
			func() fakeSession { s := mkSession("ses-c-child", dirC, "p-c"); s.ParentID = "ses-c1"; return s }(),
		}},
	}

	servers := map[string]*httptest.Server{}
	for dir, fake := range fakes {
		srv := httptest.NewServer(fake.handler())
		servers[dir] = srv
		t.Cleanup(srv.Close)
	}

	bm := host.NewOpenCodeBackendManager()
	defer bm.Shutdown()

	mgr := bm.ServerManager()
	mgr.SetStartServerFn(func(ctx context.Context, projectDir string) (*agent.OpenCodeServer, error) {
		srv, ok := servers[projectDir]
		if !ok {
			return nil, fmt.Errorf("no fake server registered for %s", projectDir)
		}
		return &agent.OpenCodeServer{
			URL:        srv.URL,
			ProjectDir: projectDir,
			StartedAt:  time.Now(),
		}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Run(ctx)

	got, err := bm.DiscoverSessions(ctx, dirA)
	if err != nil {
		t.Fatalf("DiscoverSessions: %v", err)
	}

	gotIDs := map[string]bool{}
	for _, s := range got {
		gotIDs[s.ID] = true
	}
	wantIDs := []string{"ses-a1", "ses-a2", "ses-b1", "ses-c1", "ses-c2", "ses-c3"}
	for _, id := range wantIDs {
		if !gotIDs[id] {
			t.Errorf("missing session %s; got=%v", id, gotIDs)
		}
	}
	if gotIDs["ses-c-child"] {
		t.Errorf("child session ses-c-child should have been filtered out")
	}
	if len(got) != len(wantIDs) {
		t.Errorf("got %d sessions, want %d (got=%v)", len(got), len(wantIDs), gotIDs)
	}

	// Each project's server should have been hit exactly once for /session.
	for dir, fake := range fakes {
		hits := fake.sessionListHits.Load()
		if hits != 1 {
			t.Errorf("project %s: /session hits = %d, want 1", dir, hits)
		}
	}
}

// TestDiscoverSessionsRequestsHighPaginationLimit guards against opencode's
// default /session page size of 100. clankd users routinely have hundreds of
// sessions per project; without an explicit high limit we silently truncate.
func TestDiscoverSessionsRequestsHighPaginationLimit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	allProjects := []fakeProject{{ID: "p", Worktree: dir, Time: fakeProjectTime{Created: 1}}}

	// Build 250 sessions — enough to demonstrate that a default limit of 100
	// would silently lose data.
	sessions := make([]fakeSession, 0, 250)
	for i := 0; i < 250; i++ {
		sessions = append(sessions, fakeSession{
			ID: fmt.Sprintf("ses-%d", i), Directory: dir, ProjectID: "p",
			Title: "t", Version: "v1",
			Time: fakeSessionTime{Created: 1, Updated: 2},
		})
	}

	fake := &fakeOpencodeServer{t: t, projectDir: dir, allProjects: allProjects, mySessions: sessions}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	bm := host.NewOpenCodeBackendManager()
	defer bm.Shutdown()

	mgr := bm.ServerManager()
	mgr.SetStartServerFn(func(ctx context.Context, projectDir string) (*agent.OpenCodeServer, error) {
		return &agent.OpenCodeServer{URL: srv.URL, ProjectDir: projectDir, StartedAt: time.Now()}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Run(ctx)

	got, err := bm.DiscoverSessions(ctx, dir)
	if err != nil {
		t.Fatalf("DiscoverSessions: %v", err)
	}
	if len(got) != 250 {
		t.Errorf("got %d sessions, want 250 — pagination likely truncated", len(got))
	}
	limit, _ := fake.lastLimit.Load().(string)
	if limit == "" {
		t.Errorf("expected /session request to set a limit query param, got empty")
	}
	if n, err := strconv.Atoi(limit); err != nil || n < 250 {
		t.Errorf("/session limit=%q must be >= 250 to fit all sessions", limit)
	}
}
