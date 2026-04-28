package hubmux

import (
	"fmt"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	clanksync "github.com/acksell/clank/internal/sync"
)

// registerSync wires the hub-to-hub sync endpoints. Only called when
// a sync.Receiver is attached via Mux.WithSync.
func (m *Mux) registerSync(mx *http.ServeMux) {
	mx.HandleFunc("POST /sync/repos/{repo_key}/bundle", m.handleReceiveBundle)
	mx.HandleFunc("GET /sync/repos", m.handleListSyncedRepos)
	// Prefix route: catches /sync/repos/{repo_key}/git/info/refs,
	// /git-upload-pack, etc. — git-http-backend handles routing inside.
	// Go ServeMux dispatches the most specific prefix match, so this
	// does not shadow /bundle (different second segment).
	mx.HandleFunc("/sync/repos/{repo_key}/git/", m.handleGitHTTPBackend)
}

// repoKeyRegexp matches the hex SHA-256 keys produced by sync.RepoKey.
// Stricter than safeRepoKey in the mirror package — the wire-level keys
// are always hashed, never raw URLs, so we can lock the surface down to
// exactly that shape.
var repoKeyRegexp = regexp.MustCompile(`^[0-9a-f]{64}$`)

// handleReceiveBundle accepts a streamed git bundle for a repo+branch.
//
// Wire shape (see plan / docs):
//
//	POST /sync/repos/{repo_key}/bundle
//	Headers:
//	  X-Clank-Branch:     <name>           (required)
//	  X-Clank-Tip-SHA:    <sha>            (optional, verified after unbundle)
//	  X-Clank-Base-SHA:   <sha>            (optional, informational)
//	  X-Clank-Remote-URL: <url>            (required, for display)
//	Body: raw bundle bytes (streamed)
func (m *Mux) handleReceiveBundle(w http.ResponseWriter, r *http.Request) {
	repoKey := r.PathValue("repo_key")
	if !repoKeyRegexp.MatchString(repoKey) {
		writeBadRequest(w, "invalid repo_key (want 64-char hex SHA-256)")
		return
	}
	branch := r.Header.Get("X-Clank-Branch")
	if branch == "" {
		writeBadRequest(w, "missing X-Clank-Branch header")
		return
	}
	remoteURL := r.Header.Get("X-Clank-Remote-URL")
	if remoteURL == "" {
		writeBadRequest(w, "missing X-Clank-Remote-URL header")
		return
	}
	tipSHA := r.Header.Get("X-Clank-Tip-SHA")
	baseSHA := r.Header.Get("X-Clank-Base-SHA")

	defer r.Body.Close()
	err := m.sync.ReceiveBundle(r.Context(), clanksync.ReceiveBundleRequest{
		RepoKey:   repoKey,
		RemoteURL: remoteURL,
		Branch:    branch,
		TipSHA:    tipSHA,
		BaseSHA:   baseSHA,
		Bundle:    r.Body,
	})
	if err != nil {
		writeInternal(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListSyncedRepos returns the cloud-hub's view of all synced
// repos, including their branches and last-updated timestamps. Used by
// CLI/mobile clients to know what's available before requesting a
// session on a synced branch.
func (m *Mux) handleListSyncedRepos(w http.ResponseWriter, r *http.Request) {
	repos, err := m.sync.ListSyncedRepos(r.Context())
	if err != nil {
		writeInternal(w, err)
		return
	}
	if repos == nil {
		repos = []clanksync.SyncedRepoView{}
	}
	m.writeJSON(w, http.StatusOK, repos)
}

// gitHTTPBackend resolves once. Lazily, to avoid forcing tests that
// don't exercise the smart-HTTP path to have git installed at a
// specific location.
var (
	gitHTTPBackendOnce sync.Once
	gitHTTPBackendBin  string
	gitHTTPBackendErr  error
)

func resolveGitHTTPBackend() (string, error) {
	gitHTTPBackendOnce.Do(func() {
		out, err := exec.Command("git", "--exec-path").Output()
		if err != nil {
			gitHTTPBackendErr = fmt.Errorf("git --exec-path: %w", err)
			return
		}
		bin := filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
		if _, err := os.Stat(bin); err != nil {
			gitHTTPBackendErr = fmt.Errorf("git-http-backend not found at %s: %w", bin, err)
			return
		}
		gitHTTPBackendBin = bin
	})
	return gitHTTPBackendBin, gitHTTPBackendErr
}

// handleGitHTTPBackend serves the per-repo bare mirror over smart-HTTP
// by delegating to git-http-backend as CGI. Sandboxes use this to clone
// the repo (and any unpushed branches the laptop has bundled in).
//
// The cloud hub is the *only* git source the sandbox needs. Auth is
// the bearer middleware in front of the whole hub — we don't add a
// second auth layer here. git-http-backend is configured read-only
// (no upload-archive, no receive-pack) via env.
func (m *Mux) handleGitHTTPBackend(w http.ResponseWriter, r *http.Request) {
	repoKey := r.PathValue("repo_key")
	if !repoKeyRegexp.MatchString(repoKey) {
		writeBadRequest(w, "invalid repo_key (want 64-char hex SHA-256)")
		return
	}
	mirrorBare := m.sync.MirrorPathFor(repoKey)
	if mirrorBare == "" {
		http.Error(w, "repo not synced", http.StatusNotFound)
		return
	}

	bin, err := resolveGitHTTPBackend()
	if err != nil {
		writeInternal(w, err)
		return
	}

	// Strip "/sync/repos/{repo_key}/git" so the path git-http-backend
	// sees starts with "/repo.git/<verb>" and resolves under
	// GIT_PROJECT_ROOT = .../<repo_key>.
	prefix := "/sync/repos/" + repoKey + "/git"
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	if rest == "" {
		rest = "/"
	}
	cgiURL := *r.URL
	cgiURL.Path = "/repo.git" + rest

	rewritten := r.Clone(r.Context())
	rewritten.URL = &cgiURL

	repoParent := filepath.Dir(mirrorBare) // .../<repo_key>
	handler := &cgi.Handler{
		Path: bin,
		Dir:  repoParent,
		Env: []string{
			"GIT_PROJECT_ROOT=" + repoParent,
			"GIT_HTTP_EXPORT_ALL=1",
		},
		InheritEnv: []string{"PATH"},
	}
	handler.ServeHTTP(w, rewritten)
}
