package hostmux

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/acksell/clank/pkg/sync/checkpoint"
)

// syncErrorCodes are the typed sentinels the gateway uses to decide
// whether a sync-related failure is retryable.
const (
	syncErrURLExpired    = "url_expired"    // S3 returned 403/expired
	syncErrS3Unreachable = "s3_unreachable" // network error reaching object storage
	syncErrApplyFailed   = "apply_failed"   // checkpoint.Apply returned an error
	syncErrBadManifest   = "bad_manifest"   // manifest JSON couldn't be parsed
	syncErrBadRequest    = "bad_request"    // missing/invalid URL fields
	syncErrBuildFailed   = "build_failed"   // checkpoint.Builder error
	syncErrUploadFailed  = "upload_failed"  // PUT to S3 failed (non-403)
	syncErrUnknownBuild  = "unknown_build"  // build_id not found / expired
)

// syncURLBundleFetchTimeout caps each bundle GET so a slow S3 doesn't
// hang the apply. The presigned URL TTL (5m) is the upper bound on the
// whole flow; one bundle should fit well inside that.
const syncURLBundleFetchTimeout = 4 * time.Minute

// buildExpiry bounds how long an unconsumed sprite build sits on disk.
// Pull-back builds typically upload within seconds; this is just a
// safety net against gateway crashes between /sync/build and the
// matching /sync/builds/{id}/upload call.
const buildExpiry = 30 * time.Minute

func validRepoSlug(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	if strings.ContainsAny(s, "/\\") {
		return false
	}
	if strings.Contains(s, "..") {
		return false
	}
	return true
}

// workRoot resolves $HOME/work for the sandbox user. clank-host runs as
// the same user that owns the volume, so $HOME is the right anchor.
func workRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("user home: %w", err)
	}
	return filepath.Join(home, "work"), nil
}

// applyFromURLsRequest is the JSON body of POST /sync/apply-from-urls.
type applyFromURLsRequest struct {
	Repo           string `json:"repo"`
	ManifestURL    string `json:"manifest_url"`
	HeadCommitURL  string `json:"head_commit_url"`
	IncrementalURL string `json:"incremental_url"`
}

// handleSyncApplyFromURLs is the pull-based counterpart to
// handleSyncApply: the gateway hands the sandbox presigned GET URLs
// for the checkpoint blobs and the sandbox fetches them itself from
// object storage. Bundle bytes never traverse the gateway's memory.
//
// Returns 204 on success. Errors are JSON {code, error}; code is one
// of url_expired / s3_unreachable / apply_failed / bad_manifest /
// bad_request so the gateway can decide whether to retry.
//
// TODO(coderabbit): bound manifest via io.LimitReader before ReadAll;
// stream head/incremental bundles into checkpoint.Apply rather than
// buffering. https://github.com/Acksell/clank/pull/17#discussion_r3227672622
func (m *Mux) handleSyncApplyFromURLs(w http.ResponseWriter, r *http.Request) {
	var req applyFromURLsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Code: syncErrBadRequest, Error: "decode body: " + err.Error()})
		return
	}
	if !validRepoSlug(req.Repo) {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "bad_repo", Error: "repo must be a single non-empty path segment without '..' or '/'"})
		return
	}
	if req.ManifestURL == "" || req.HeadCommitURL == "" || req.IncrementalURL == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Code: syncErrBadRequest, Error: "manifest_url, head_commit_url, and incremental_url are all required"})
		return
	}

	workDir, err := workRoot()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errResp{Code: "work_root", Error: err.Error()})
		return
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, errResp{Code: "mkdir_work", Error: err.Error()})
		return
	}
	target := filepath.Join(workDir, req.Repo)

	cli := &http.Client{Timeout: syncURLBundleFetchTimeout}

	manifestBytes, status, code, err := fetchURL(r.Context(), cli, req.ManifestURL)
	if err != nil {
		writeJSON(w, status, errResp{Code: code, Error: "fetch manifest: " + err.Error()})
		return
	}
	if len(manifestBytes) > 1<<20 {
		writeJSON(w, http.StatusBadRequest, errResp{Code: syncErrBadManifest, Error: "manifest exceeds 1MiB"})
		return
	}
	var manifest checkpoint.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Code: syncErrBadManifest, Error: "parse manifest: " + err.Error()})
		return
	}
	headBytes, status, code, err := fetchURL(r.Context(), cli, req.HeadCommitURL)
	if err != nil {
		writeJSON(w, status, errResp{Code: code, Error: "fetch head bundle: " + err.Error()})
		return
	}
	incrBytes, status, code, err := fetchURL(r.Context(), cli, req.IncrementalURL)
	if err != nil {
		writeJSON(w, status, errResp{Code: code, Error: "fetch incremental bundle: " + err.Error()})
		return
	}

	if err := checkpoint.Apply(r.Context(), target, &manifest, bytes.NewReader(headBytes), bytes.NewReader(incrBytes)); err != nil {
		writeJSON(w, http.StatusInternalServerError, errResp{Code: syncErrApplyFailed, Error: err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- pull-back build/upload/delete ---------------------------------
//
// The gateway orchestrates a pull-back migration with three calls:
//
//   1. POST /sync/build?repo=<id>         -> sprite builds bundles to
//                                            local disk, returns metadata
//                                            and a build_id.
//   2. POST /sync/builds/{id}/upload      -> sprite PUTs the bundles to
//                                            S3 via presigned URLs the
//                                            gateway minted from the
//                                            metadata returned in step 1.
//   3. DELETE /sync/builds/{id}           -> idempotent cleanup. Upload
//                                            also cleans up on success;
//                                            DELETE recovers orphans
//                                            when the gateway crashes
//                                            between steps.
//
// Sprite holds NO sync-server credentials and makes NO outbound calls
// to clankd. The only outbound it makes is during step 2, to S3, via
// short-lived presigned URLs the gateway provided.

// spriteBuild is one in-progress build's on-disk state, keyed by
// build_id in Mux.builds. Manifest is unmarshalled metadata only —
// the canonical bytes uploaded to S3 are re-serialized at upload time
// after stamping the gateway-assigned checkpoint_id.
type spriteBuild struct {
	headCommitBundle  string // path under /tmp; deleted on upload/delete/expiry
	incrementalBundle string // ditto
	manifest          *checkpoint.Manifest
	createdAt         time.Time
}

// buildResponse is the body returned by POST /sync/build. The gateway
// passes these metadata fields back to its sync server's CreateCheckpoint
// to mint presigned PUT URLs.
type buildResponse struct {
	BuildID           string `json:"build_id"`
	HeadCommit        string `json:"head_commit"`
	HeadRef           string `json:"head_ref"`
	IndexTree         string `json:"index_tree"`
	WorktreeTree      string `json:"worktree_tree"`
	IncrementalCommit string `json:"incremental_commit"`
}

// handleSyncBuild builds a two-bundle checkpoint of ~/work/<repo> to
// local disk and returns its metadata. No credentials in body; gateway
// is trusted by virtue of the inbound auth token.
func (m *Mux) handleSyncBuild(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	if !validRepoSlug(repo) {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "bad_repo", Error: "repo must be a single non-empty path segment without '..' or '/'"})
		return
	}
	workDir, err := workRoot()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errResp{Code: "work_root", Error: err.Error()})
		return
	}
	repoPath := filepath.Join(workDir, repo)
	if _, statErr := os.Stat(repoPath); statErr != nil {
		writeJSON(w, http.StatusNotFound, errResp{Code: "missing_repo", Error: "repo not found at " + repoPath})
		return
	}

	// HostID is unknown to the sprite without callback creds, so stamp
	// a generic "sprite" stand-in here. The canonical CreatedBy gets
	// re-stamped at upload time when the gateway supplies the real
	// checkpoint_id (and could supply a host_id too — see the comment
	// at handleSyncBuildsUpload).
	buildID := ulid.Make().String()
	builder := checkpoint.NewBuilder(repoPath, "sprite")
	res, err := builder.Build(r.Context(), buildID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errResp{Code: syncErrBuildFailed, Error: err.Error()})
		return
	}

	m.builds.add(buildID, &spriteBuild{
		headCommitBundle:  res.HeadCommitBundle,
		incrementalBundle: res.IncrementalBundle,
		manifest:          res.Manifest,
		createdAt:         time.Now(),
	})

	writeJSON(w, http.StatusOK, buildResponse{
		BuildID:           buildID,
		HeadCommit:        res.Manifest.HeadCommit,
		HeadRef:           res.Manifest.HeadRef,
		IndexTree:         res.Manifest.IndexTree,
		WorktreeTree:      res.Manifest.WorktreeTree,
		IncrementalCommit: res.Manifest.IncrementalCommit,
	})
}

// uploadBuildRequest is the JSON body of POST /sync/builds/{id}/upload.
// The gateway has minted these URLs after calling sync.CreateCheckpoint
// with the metadata returned by /sync/build.
type uploadBuildRequest struct {
	CheckpointID     string `json:"checkpoint_id"`
	ManifestPutURL   string `json:"manifest_put_url"`
	HeadCommitPutURL string `json:"head_commit_put_url"`
	IncrementalPutURL string `json:"incremental_put_url"`
}

// handleSyncBuildsUpload finalizes a build by uploading its three blobs
// to the presigned PUT URLs supplied by the gateway. On success the
// build is deleted from local disk; on failure it stays so the gateway
// can retry with fresh URLs.
func (m *Mux) handleSyncBuildsUpload(w http.ResponseWriter, r *http.Request) {
	buildID := r.PathValue("id")
	if buildID == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Code: syncErrBadRequest, Error: "build id missing"})
		return
	}
	var req uploadBuildRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Code: syncErrBadRequest, Error: "decode body: " + err.Error()})
		return
	}
	if req.CheckpointID == "" || req.ManifestPutURL == "" || req.HeadCommitPutURL == "" || req.IncrementalPutURL == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Code: syncErrBadRequest, Error: "checkpoint_id, manifest_put_url, head_commit_put_url, incremental_put_url are all required"})
		return
	}

	b := m.builds.get(buildID)
	if b == nil {
		writeJSON(w, http.StatusNotFound, errResp{Code: syncErrUnknownBuild, Error: "build " + buildID + " not found (expired or already uploaded)"})
		return
	}

	// Stamp the canonical checkpoint_id into the manifest before
	// upload. The /sync/build step couldn't know it — sync assigns
	// the ID server-side after the gateway calls CreateCheckpoint.
	b.manifest.CheckpointID = req.CheckpointID
	manifestBytes, err := b.manifest.Marshal()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errResp{Code: syncErrBuildFailed, Error: "marshal manifest: " + err.Error()})
		return
	}

	cli := &http.Client{Timeout: syncURLBundleFetchTimeout}

	if status, code, err := uploadFile(r.Context(), cli, req.HeadCommitPutURL, b.headCommitBundle); err != nil {
		writeJSON(w, status, errResp{Code: code, Error: "upload head bundle: " + err.Error()})
		return
	}
	if status, code, err := uploadFile(r.Context(), cli, req.IncrementalPutURL, b.incrementalBundle); err != nil {
		writeJSON(w, status, errResp{Code: code, Error: "upload incremental bundle: " + err.Error()})
		return
	}
	if status, code, err := uploadBytes(r.Context(), cli, req.ManifestPutURL, manifestBytes, "application/json"); err != nil {
		writeJSON(w, status, errResp{Code: code, Error: "upload manifest: " + err.Error()})
		return
	}

	m.builds.remove(buildID)
	w.WriteHeader(http.StatusNoContent)
}

// handleSyncBuildsDelete removes a build's on-disk artifacts.
// Idempotent — returns 204 whether or not the build still exists.
func (m *Mux) handleSyncBuildsDelete(w http.ResponseWriter, r *http.Request) {
	buildID := r.PathValue("id")
	if buildID == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Code: syncErrBadRequest, Error: "build id missing"})
		return
	}
	m.builds.remove(buildID)
	w.WriteHeader(http.StatusNoContent)
}

// --- build store -----------------------------------------------------

// spriteBuildStore is the in-memory map of build_id → on-disk
// artifacts. A background reaper trims entries older than buildExpiry
// so a gateway that crashed between build and upload doesn't leak
// disk forever.
type spriteBuildStore struct {
	mu     sync.Mutex
	builds map[string]*spriteBuild
}

func newSpriteBuildStore() *spriteBuildStore {
	s := &spriteBuildStore{builds: map[string]*spriteBuild{}}
	go s.reapLoop(buildExpiry)
	return s
}

func (s *spriteBuildStore) add(id string, b *spriteBuild) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.builds[id] = b
}

func (s *spriteBuildStore) get(id string) *spriteBuild {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.builds[id]
}

func (s *spriteBuildStore) remove(id string) {
	s.mu.Lock()
	b := s.builds[id]
	delete(s.builds, id)
	s.mu.Unlock()
	if b != nil {
		removeBuildFiles(b)
	}
}

// reapLoop periodically scans for builds older than maxAge and frees
// their on-disk artifacts. Runs forever; no shutdown signal because
// the sprite process exit reclaims everything anyway.
func (s *spriteBuildStore) reapLoop(maxAge time.Duration) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for range t.C {
		cutoff := time.Now().Add(-maxAge)
		s.mu.Lock()
		var stale []*spriteBuild
		for id, b := range s.builds {
			if b.createdAt.Before(cutoff) {
				stale = append(stale, b)
				delete(s.builds, id)
			}
		}
		s.mu.Unlock()
		for _, b := range stale {
			removeBuildFiles(b)
		}
	}
}

func removeBuildFiles(b *spriteBuild) {
	if b.headCommitBundle != "" {
		_ = os.Remove(b.headCommitBundle)
	}
	if b.incrementalBundle != "" {
		_ = os.Remove(b.incrementalBundle)
	}
}

// --- helpers ---------------------------------------------------------

// fetchURL GETs blobURL and returns its body, an HTTP status code to
// return on error, and a sync-error sentinel for the gateway's
// retry-decision logic.
func fetchURL(ctx context.Context, cli *http.Client, blobURL string) (body []byte, errStatus int, errCode string, err error) {
	parsed, perr := url.Parse(blobURL)
	if perr != nil || parsed.Scheme == "" {
		return nil, http.StatusBadRequest, syncErrBadRequest, fmt.Errorf("invalid url: %v", perr)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, blobURL, nil)
	if err != nil {
		return nil, http.StatusBadRequest, syncErrBadRequest, err
	}
	resp, err := cli.Do(req)
	if err != nil {
		if _, ok := err.(net.Error); ok || strings.Contains(err.Error(), "dial ") {
			return nil, http.StatusBadGateway, syncErrS3Unreachable, err
		}
		return nil, http.StatusBadGateway, syncErrS3Unreachable, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		return nil, http.StatusBadGateway, syncErrURLExpired, fmt.Errorf("403 from object storage")
	}
	if resp.StatusCode/100 != 2 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, http.StatusBadGateway, syncErrApplyFailed, fmt.Errorf("blob GET %d: %s", resp.StatusCode, strings.TrimSpace(string(preview)))
	}
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, http.StatusBadGateway, syncErrS3Unreachable, err
	}
	return body, 0, "", nil
}

// uploadFile streams a local file to a presigned PUT URL. Returns an
// HTTP status + error code on failure so the handler surfaces typed
// errors to the gateway.
func uploadFile(ctx context.Context, cli *http.Client, putURL, path string) (int, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return http.StatusInternalServerError, syncErrUploadFailed, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return http.StatusInternalServerError, syncErrUploadFailed, fmt.Errorf("stat %s: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, putURL, f)
	if err != nil {
		return http.StatusBadRequest, syncErrBadRequest, err
	}
	req.ContentLength = stat.Size()
	req.Header.Set("Content-Type", "application/octet-stream")
	return doUpload(cli, req)
}

func uploadBytes(ctx context.Context, cli *http.Client, putURL string, data []byte, contentType string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, putURL, bytes.NewReader(data))
	if err != nil {
		return http.StatusBadRequest, syncErrBadRequest, err
	}
	req.ContentLength = int64(len(data))
	req.Header.Set("Content-Type", contentType)
	return doUpload(cli, req)
}

func doUpload(cli *http.Client, req *http.Request) (int, string, error) {
	resp, err := cli.Do(req)
	if err != nil {
		if _, ok := err.(net.Error); ok || strings.Contains(err.Error(), "dial ") {
			return http.StatusBadGateway, syncErrS3Unreachable, err
		}
		return http.StatusBadGateway, syncErrS3Unreachable, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		return http.StatusBadGateway, syncErrURLExpired, fmt.Errorf("403 from object storage")
	}
	if resp.StatusCode/100 != 2 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return http.StatusBadGateway, syncErrUploadFailed, fmt.Errorf("PUT %d: %s", resp.StatusCode, strings.TrimSpace(string(preview)))
	}
	return 0, "", nil
}
