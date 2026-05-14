package sync_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	clanksync "github.com/acksell/clank/pkg/sync"
	"github.com/acksell/clank/pkg/sync/storage"
)

// newSessionsTestServer wires up an in-process sync.Server with a
// memSyncStore + storage.Memory. Returns the server plus the user/
// worktree/checkpoint identifiers it seeded.
func newSessionsTestServer(t *testing.T) (*clanksync.Server, *memSyncStore, *storage.Memory, string, string, string) {
	t.Helper()
	const userID = "user-A"
	const worktreeID = "wt-sessions"
	const checkpointID = "ck-sessions"

	st := newMemSyncStore()
	mem := storage.NewMemory()
	t.Cleanup(mem.Close)

	// Seed worktree + checkpoint manually so we don't depend on the
	// CreateCheckpoint flow.
	now := time.Now().UTC()
	if err := st.InsertWorktree(context.Background(), clanksync.Worktree{
		ID: worktreeID, UserID: userID,
		OwnerKind: clanksync.OwnerKindLocal, OwnerID: "",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertCheckpoint(context.Background(), clanksync.Checkpoint{
		ID:                checkpointID,
		WorktreeID:        worktreeID,
		HeadCommit:        "deadbeef",
		IndexTree:         "1111",
		WorktreeTree:      "2222",
		IncrementalCommit: "3333",
		CreatedAt:         now,
		CreatedBy:         "test",
	}); err != nil {
		t.Fatal(err)
	}

	srv, err := clanksync.NewServer(clanksync.Config{
		Store:      st,
		Storage:    mem,
		PresignTTL: time.Minute,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return srv, st, mem, userID, worktreeID, checkpointID
}

// TestPresignSessionPuts_HappyPath: minting URLs returns one per
// session plus the manifest URL, all addressable in the storage.
func TestPresignSessionPuts_HappyPath(t *testing.T) {
	t.Parallel()
	srv, _, mem, userID, _, checkpointID := newSessionsTestServer(t)

	res, err := srv.PresignSessionPuts(context.Background(), userID, clanksync.SessionPresignRequest{
		CheckpointID: checkpointID,
		SessionIDs:   []string{"01H0", "01H1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.CheckpointID != checkpointID {
		t.Errorf("CheckpointID = %q, want %q", res.CheckpointID, checkpointID)
	}
	if len(res.SessionPutURLs) != 2 {
		t.Fatalf("want 2 session URLs, got %d", len(res.SessionPutURLs))
	}
	if res.SessionPutURLs["01H0"] == "" || res.SessionPutURLs["01H1"] == "" {
		t.Errorf("missing per-session PUT URLs: %+v", res.SessionPutURLs)
	}
	if res.SessionManifestPutURL == "" {
		t.Errorf("missing manifest PUT URL")
	}

	// Upload to the per-session URL and verify it lands in storage.
	uploadTo(t, res.SessionPutURLs["01H0"], []byte(`{"info":{"id":"ses_a"}}`))
	keys := mem.Keys()
	want := "checkpoints/" + userID + "/wt-sessions/" + checkpointID + "/sessions/01H0.json"
	var found bool
	for _, k := range keys {
		if k == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected key %s in storage, got %v", want, keys)
	}
}

// TestPresignSessionPuts_WrongTenantForbidden: a different user
// trying to presign for someone else's checkpoint gets forbidden.
func TestPresignSessionPuts_WrongTenantForbidden(t *testing.T) {
	t.Parallel()
	srv, _, _, _, _, checkpointID := newSessionsTestServer(t)

	_, err := srv.PresignSessionPuts(context.Background(), "user-B", clanksync.SessionPresignRequest{
		CheckpointID: checkpointID,
		SessionIDs:   []string{"01H0"},
	})
	if err == nil {
		t.Fatal("expected forbidden error")
	}
}

// TestPresignSessionPuts_EmptyCheckpointID: input validation.
func TestPresignSessionPuts_EmptyCheckpointID(t *testing.T) {
	t.Parallel()
	srv, _, _, userID, _, _ := newSessionsTestServer(t)

	_, err := srv.PresignSessionPuts(context.Background(), userID, clanksync.SessionPresignRequest{
		SessionIDs: []string{"01H0"},
	})
	if !errors.Is(err, clanksync.ErrInvalidRequest) {
		t.Errorf("expected ErrInvalidRequest, got %v", err)
	}
}

// TestDownloadSessionURLs_RoundTrip: presign → upload → mark
// uploaded → download → assert bytes match.
func TestDownloadSessionURLs_RoundTrip(t *testing.T) {
	t.Parallel()
	srv, st, _, userID, _, checkpointID := newSessionsTestServer(t)

	// Presign + upload session blob and manifest.
	presign, err := srv.PresignSessionPuts(context.Background(), userID, clanksync.SessionPresignRequest{
		CheckpointID: checkpointID,
		SessionIDs:   []string{"01H0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionBlob := []byte(`{"info":{"id":"ses_a"}}`)
	manifestBlob := []byte(`{"version":1}`)
	uploadTo(t, presign.SessionPutURLs["01H0"], sessionBlob)
	uploadTo(t, presign.SessionManifestPutURL, manifestBlob)

	// Mark checkpoint as uploaded — required for download.
	if err := st.MarkCheckpointUploaded(context.Background(), checkpointID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	dl, err := srv.DownloadSessionURLs(context.Background(), userID, checkpointID, []string{"01H0"})
	if err != nil {
		t.Fatal(err)
	}
	if dl.CheckpointID != checkpointID {
		t.Errorf("CheckpointID = %q", dl.CheckpointID)
	}
	if dl.SessionManifestGetURL == "" {
		t.Errorf("missing manifest GET URL")
	}
	if dl.SessionGetURLs["01H0"] == "" {
		t.Fatalf("missing session GET URL")
	}

	// GET each URL and verify the bytes round-trip.
	body := getFrom(t, dl.SessionGetURLs["01H0"])
	if string(body) != string(sessionBlob) {
		t.Errorf("session blob mismatch:\n got %s\n want %s", body, sessionBlob)
	}
	body = getFrom(t, dl.SessionManifestGetURL)
	if string(body) != string(manifestBlob) {
		t.Errorf("manifest blob mismatch:\n got %s\n want %s", body, manifestBlob)
	}
}

// TestDownloadSessionURLs_NotUploadedRejected: download must reject
// checkpoints that haven't been committed yet.
func TestDownloadSessionURLs_NotUploadedRejected(t *testing.T) {
	t.Parallel()
	srv, _, _, userID, _, checkpointID := newSessionsTestServer(t)

	_, err := srv.DownloadSessionURLs(context.Background(), userID, checkpointID, []string{"01H0"})
	if err == nil {
		t.Fatal("expected error for un-uploaded checkpoint")
	}
	if !strings.Contains(err.Error(), "not yet uploaded") {
		t.Errorf("expected 'not yet uploaded' error, got %v", err)
	}
}

// --- helpers (uploadTo defined in checkpoints_handler_test.go; getFrom new) ---

func getFrom(t *testing.T, url string) []byte {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
