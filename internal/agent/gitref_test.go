package agent

import "testing"

func TestRepoDisplayName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ref  GitRef
		want string
	}{
		{"https remote", GitRef{RemoteURL: "https://github.com/acksell/clank.git"}, "clank"},
		{"scp remote", GitRef{RemoteURL: "git@github.com:acksell/clank.git"}, "clank"},
		{"file URL", GitRef{RemoteURL: "file:///srv/git/foo.git"}, "foo"},
		{"local abs", GitRef{LocalPath: "/Users/x/code/clank"}, "clank"},
		{"local trailing slash", GitRef{LocalPath: "/Users/x/code/clank/"}, "clank"},
		{"both prefers remote", GitRef{LocalPath: "/Users/x/code/wrong", RemoteURL: "https://github.com/acksell/clank.git"}, "clank"},
		{"empty ref", GitRef{}, ""},
		{"local relative", GitRef{LocalPath: "rel/path"}, ""},
		{"remote bad falls to local", GitRef{LocalPath: "/Users/x/code/clank", RemoteURL: "::::"}, "clank"},
		{"remote bad no local", GitRef{RemoteURL: "::::"}, ""},
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
		{"both set ok", GitRef{LocalPath: "/x", RemoteURL: "https://github.com/acksell/clank.git"}, false},
		{"local ok", GitRef{LocalPath: "/tmp/repo"}, false},
		{"local relative", GitRef{LocalPath: "rel"}, true},
		{"remote https", GitRef{RemoteURL: "https://github.com/acksell/clank.git"}, false},
		{"remote scp", GitRef{RemoteURL: "git@github.com:acksell/clank.git"}, false},
		{"remote bad", GitRef{RemoteURL: "not a url"}, true},
		{"both but local relative", GitRef{LocalPath: "rel", RemoteURL: "https://github.com/x/y.git"}, true},
		{"both but remote bad", GitRef{LocalPath: "/x", RemoteURL: "not a url"}, true},
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
	// Prefers RemoteURL when both set: stable across machines.
	a := GitRef{LocalPath: "/Users/alice/code/clank", RemoteURL: "https://github.com/acksell/clank.git", WorktreeBranch: "main"}
	b := GitRef{LocalPath: "/home/bob/src/clank", RemoteURL: "https://github.com/acksell/clank.git", WorktreeBranch: "main"}
	if RepoKey(a) != RepoKey(b) {
		t.Fatalf("expected matching keys for same RemoteURL+branch, got %q vs %q", RepoKey(a), RepoKey(b))
	}
	// Local-only refs key by LocalPath.
	c := GitRef{LocalPath: "/x", WorktreeBranch: "feat"}
	if RepoKey(c) == "" {
		t.Fatal("expected non-empty key for local-only ref")
	}
	if RepoKey(GitRef{}) != "" {
		t.Fatal("expected empty key for empty ref")
	}
}
