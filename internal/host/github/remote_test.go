package github

import (
	"errors"
	"testing"
)

func TestParseGitHubRemote(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		url       string
		wantOwner string
		wantRepo  string
		wantErr   error
	}{
		{name: "https with .git", url: "https://github.com/acme/api.git", wantOwner: "acme", wantRepo: "api"},
		{name: "https without .git", url: "https://github.com/acme/api", wantOwner: "acme", wantRepo: "api"},
		{name: "https trailing slash", url: "https://github.com/acme/api/", wantOwner: "acme", wantRepo: "api"},
		{name: "scp-style with .git", url: "git@github.com:acme/api.git", wantOwner: "acme", wantRepo: "api"},
		{name: "scp-style without .git", url: "git@github.com:acme/api", wantOwner: "acme", wantRepo: "api"},
		{name: "ssh full url", url: "ssh://git@github.com/acme/api.git", wantOwner: "acme", wantRepo: "api"},
		{name: "https with userinfo", url: "https://user:tok@github.com/acme/api.git", wantOwner: "acme", wantRepo: "api"},
		{name: "leading/trailing whitespace", url: "  https://github.com/acme/api.git  ", wantOwner: "acme", wantRepo: "api"},

		// rejected — wrong host
		{name: "gitlab", url: "https://gitlab.com/acme/api.git", wantErr: ErrNotGitHubRemote},
		{name: "self-hosted gitea", url: "https://gitea.example.com/acme/api.git", wantErr: ErrNotGitHubRemote},
		{name: "scp-style non-github", url: "git@gitlab.com:acme/api.git", wantErr: ErrNotGitHubRemote},
		{name: "GHE (deferred)", url: "https://github.acme.com/acme/api.git", wantErr: ErrNotGitHubRemote},

		// rejected — extra path segments (e.g. tree-URL paste)
		{name: "tree url", url: "https://github.com/acme/api/tree/main", wantErr: ErrNotGitHubRemote},
		{name: "blob url", url: "https://github.com/acme/api/blob/main/README.md", wantErr: ErrNotGitHubRemote},

		// rejected — malformed
		{name: "no path", url: "https://github.com", wantErr: ErrNotGitHubRemote},
		{name: "no repo", url: "https://github.com/acme", wantErr: ErrNotGitHubRemote},
		{name: "empty owner", url: "https://github.com//api.git", wantErr: ErrNotGitHubRemote},
		{name: "empty string", url: "", wantErr: ErrNotGitHubRemote},
		{name: "whitespace only", url: "   ", wantErr: ErrNotGitHubRemote},
		{name: "garbage", url: "not-a-url", wantErr: ErrNotGitHubRemote},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			owner, repo, err := ParseGitHubRemote(tc.url)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if owner != tc.wantOwner || repo != tc.wantRepo {
				t.Fatalf("got (%q, %q), want (%q, %q)", owner, repo, tc.wantOwner, tc.wantRepo)
			}
		})
	}
}
