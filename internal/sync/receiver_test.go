package sync

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/acksell/clank/internal/store"
)

func TestReceiver_PersistsAfterUnbundle(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	st, err := store.Open(filepath.Join(dir, "clank.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	root, err := NewMirrorRoot(filepath.Join(dir, "sync"))
	if err != nil {
		t.Fatalf("NewMirrorRoot: %v", err)
	}
	recv := NewReceiver(root, st, nil)

	body, tip := makeBundle(t, "feat/a", "a.txt", "alpha\n")
	const remoteURL = "https://github.com/example/proj.git"
	repoKey := RepoKey(remoteURL)

	err = recv.ReceiveBundle(context.Background(), ReceiveBundleRequest{
		RepoKey:   repoKey,
		RemoteURL: remoteURL,
		Branch:    "feat/a",
		TipSHA:    tip,
		Bundle:    bytes.NewReader(body),
	})
	if err != nil {
		t.Fatalf("ReceiveBundle: %v", err)
	}

	repos, err := recv.ListSyncedRepos(context.Background())
	if err != nil {
		t.Fatalf("ListSyncedRepos: %v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("want 1 repo, got %d", len(repos))
	}
	if repos[0].RepoKey != repoKey || repos[0].RemoteURL != remoteURL {
		t.Errorf("repo entry: %+v", repos[0])
	}
	if len(repos[0].Branches) != 1 {
		t.Fatalf("want 1 branch, got %d", len(repos[0].Branches))
	}
	if repos[0].Branches[0].Branch != "feat/a" || repos[0].Branches[0].TipSHA != tip {
		t.Errorf("branch entry: %+v", repos[0].Branches[0])
	}

	// Receive a second branch — the same repo row should remain, plus a
	// new branch row.
	body2, tip2 := makeBundle(t, "feat/b", "b.txt", "bravo\n")
	err = recv.ReceiveBundle(context.Background(), ReceiveBundleRequest{
		RepoKey:   repoKey,
		RemoteURL: remoteURL,
		Branch:    "feat/b",
		TipSHA:    tip2,
		Bundle:    bytes.NewReader(body2),
	})
	if err != nil {
		t.Fatalf("ReceiveBundle (b): %v", err)
	}

	repos, err = recv.ListSyncedRepos(context.Background())
	if err != nil {
		t.Fatalf("ListSyncedRepos (after second bundle): %v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("want still 1 repo after second branch, got %d", len(repos))
	}
	if len(repos[0].Branches) != 2 {
		t.Fatalf("want 2 branches, got %d", len(repos[0].Branches))
	}
}

func TestReceiver_RejectsMissingFields(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := NewMirrorRoot(filepath.Join(dir, "sync"))
	if err != nil {
		t.Fatalf("NewMirrorRoot: %v", err)
	}
	recv := NewReceiver(root, nil, nil)

	body, _ := makeBundle(t, "main", "x.txt", "x\n")
	cases := []ReceiveBundleRequest{
		{RemoteURL: "u", Branch: "main", Bundle: bytes.NewReader(body)},                  // missing repo_key
		{RepoKey: RepoKey("u"), RemoteURL: "u", Bundle: bytes.NewReader(body)},           // missing branch
		{RepoKey: RepoKey("u"), Branch: "main", Bundle: bytes.NewReader(body)},           // missing remote_url
		{RepoKey: RepoKey("u"), RemoteURL: "u", Branch: "main"},                          // missing bundle reader
	}
	for i, c := range cases {
		if err := recv.ReceiveBundle(context.Background(), c); err == nil {
			t.Errorf("case %d: want error, got nil", i)
		}
	}
}
