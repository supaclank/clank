package git

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCloneShallowKeepRemote verifies the import-clone shape: a depth-1
// clone that keeps .git and the origin remote (unlike the template flow,
// which strips them).
func TestCloneShallowKeepRemote(t *testing.T) {
	t.Parallel()
	// file:// (not a bare path) so git uses real transport and honors
	// --depth instead of its local hardlink optimization.
	srcURL := "file://" + initTestRepo(t)
	dst := filepath.Join(t.TempDir(), "clone")

	if err := CloneShallowKeepRemote(context.Background(), srcURL, dst, ""); err != nil {
		t.Fatalf("CloneShallowKeepRemote: %v", err)
	}

	// Working tree materialized.
	if out := run(t, dst, "git", "rev-parse", "--is-inside-work-tree"); strings.TrimSpace(out) != "true" {
		t.Errorf("not a work tree: %q", out)
	}
	// Origin remote preserved and pointing at the source.
	gotURL, err := RemoteURL(dst, "origin")
	if err != nil {
		t.Fatalf("RemoteURL: %v", err)
	}
	if gotURL != srcURL {
		t.Errorf("origin url = %q, want %q", gotURL, srcURL)
	}
	// Shallow.
	if out := run(t, dst, "git", "rev-parse", "--is-shallow-repository"); strings.TrimSpace(out) != "true" {
		t.Errorf("not shallow: %q", out)
	}
}

// TestCloneShallowKeepRemoteWithToken is the private-repo regression: the
// token must reach git via the inline credential helper so an
// authenticated HTTPS clone succeeds, and a wrong token must fail. Serves
// a real bare repo through git-http-backend behind a Basic-auth gate.
func TestCloneShallowKeepRemoteWithToken(t *testing.T) {
	t.Parallel()
	backend := gitHTTPBackendPath(t)

	// A bare repo to serve.
	src := initTestRepo(t)
	root := t.TempDir()
	run(t, root, "git", "clone", "--bare", src, filepath.Join(root, "repo.git"))

	const wantToken = "s3cr3t-token"
	// git authenticates HTTPS as Basic base64("x-access-token:<token>"):
	// the credential helper supplies that username with the token as the
	// password.
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+wantToken))

	cgiHandler := &cgi.Handler{
		Path: backend,
		Env: []string{
			"GIT_PROJECT_ROOT=" + root,
			"GIT_HTTP_EXPORT_ALL=1",
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 401 with no/invalid creds drives git to invoke the credential
		// helper and retry with Basic auth.
		if r.Header.Get("Authorization") != wantAuth {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		cgiHandler.ServeHTTP(w, r)
	}))
	defer srv.Close()

	cloneURL := srv.URL + "/repo.git"

	t.Run("valid token", func(t *testing.T) {
		dst := filepath.Join(t.TempDir(), "ok")
		if err := CloneShallowKeepRemote(context.Background(), cloneURL, dst, wantToken); err != nil {
			t.Fatalf("clone with valid token: %v", err)
		}
		// Remote URL stays clean — token must not be persisted in config.
		gotURL, err := RemoteURL(dst, "origin")
		if err != nil {
			t.Fatalf("RemoteURL: %v", err)
		}
		if gotURL != cloneURL {
			t.Errorf("origin url = %q, want %q (token must not leak into config)", gotURL, cloneURL)
		}
		if strings.Contains(gotURL, wantToken) {
			t.Errorf("token leaked into remote url: %q", gotURL)
		}
	})

	t.Run("wrong token fails", func(t *testing.T) {
		dst := filepath.Join(t.TempDir(), "bad")
		if err := CloneShallowKeepRemote(context.Background(), cloneURL, dst, "wrong-token"); err == nil {
			t.Fatal("clone with wrong token succeeded, want failure")
		}
	})
}

// gitHTTPBackendPath locates git-http-backend in git's exec-path, skipping
// the test when it isn't installed (some minimal git builds omit it).
func gitHTTPBackendPath(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skipf("git --exec-path: %v", err)
	}
	p := filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
	if _, err := exec.LookPath(p); err != nil {
		t.Skipf("git-http-backend not available at %s: %v", p, err)
	}
	return p
}
