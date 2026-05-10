package agent

import "testing"

func TestRepoDisplayName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ref  GitRef
		want string
	}{
		{"explicit display name wins", GitRef{LocalPath: "/Users/x/wrong", DisplayName: "myrepo"}, "myrepo"},
		{"local abs basename", GitRef{LocalPath: "/Users/x/code/clank"}, "clank"},
		{"local trailing slash", GitRef{LocalPath: "/Users/x/code/clank/"}, "clank"},
		{"empty ref", GitRef{}, ""},
		{"local relative ignored", GitRef{LocalPath: "rel/path"}, ""},
		{"worktree only no name", GitRef{WorktreeID: "01HXYZ"}, ""},
		{"worktree with display", GitRef{WorktreeID: "01HXYZ", DisplayName: "myrepo"}, "myrepo"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := RepoDisplayName(tc.ref); got != tc.want {
				t.Fatalf("RepoDisplayName()=%q want %q", got, tc.want)
			}
		})
	}
}

func TestGitRefValidate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		ref     GitRef
		wantErr bool
	}{
		{"empty", GitRef{}, true},
		{"local ok", GitRef{LocalPath: "/tmp/repo"}, false},
		{"local relative", GitRef{LocalPath: "rel"}, true},
		{"worktree only", GitRef{WorktreeID: "01HXYZ"}, false},
		{"both ok", GitRef{LocalPath: "/x", WorktreeID: "01HXYZ"}, false},
		{"both but local relative", GitRef{LocalPath: "rel", WorktreeID: "01HXYZ"}, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.ref.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestRepoKey(t *testing.T) {
	t.Parallel()
	// Prefers WorktreeID when set: cross-machine stable identity. Two
	// laptops with different LocalPaths but the same WorktreeID share
	// a key.
	a := GitRef{LocalPath: "/Users/alice/code/clank", WorktreeID: "01HXYZ", WorktreeBranch: "main"}
	b := GitRef{LocalPath: "/home/bob/src/clank", WorktreeID: "01HXYZ", WorktreeBranch: "main"}
	if RepoKey(a) != RepoKey(b) {
		t.Fatalf("expected matching keys for same WorktreeID+branch, got %q vs %q", RepoKey(a), RepoKey(b))
	}
	// Local-only refs key by LocalPath.
	c := GitRef{LocalPath: "/x", WorktreeBranch: "feat"}
	if RepoKey(c) == "" {
		t.Fatal("expected non-empty key for local-only ref")
	}
	if RepoKey(GitRef{}) != "" {
		t.Fatal("expected empty key for empty ref")
	}
	// Distinct WorktreeID + LocalPath produce distinct keys.
	d := GitRef{LocalPath: "/x"}
	e := GitRef{WorktreeID: "01HXYZ"}
	if RepoKey(d) == RepoKey(e) {
		t.Fatalf("expected distinct keys for distinct identifiers")
	}
}
