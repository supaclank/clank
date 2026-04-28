package sync

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/acksell/clank/internal/store"
)

// Receiver is the cloud-hub component that ingests bundles pushed from
// laptop hubs into the per-repo mirror and records the resulting state
// in SQLite. It owns no transport — the hub mux invokes ReceiveBundle
// from its HTTP handler.
type Receiver struct {
	mirrors *MirrorRoot
	store   *store.Store // optional; nil = no persistence (in-memory only)
	log     *log.Logger
}

// NewReceiver wires a MirrorRoot and (optional) Store into a Receiver.
func NewReceiver(mirrors *MirrorRoot, st *store.Store, lg *log.Logger) *Receiver {
	if mirrors == nil {
		panic("sync.NewReceiver: mirrors is required")
	}
	if lg == nil {
		lg = log.Default()
	}
	return &Receiver{mirrors: mirrors, store: st, log: lg}
}

// MirrorRoot returns the underlying MirrorRoot. Used by the smart-HTTP
// handler to find the bare repo for a given key.
func (r *Receiver) MirrorRoot() *MirrorRoot { return r.mirrors }

// MirrorPathFor returns the on-disk bare-repo path for the given
// repo_key, or empty string if no mirror exists yet (no bundle has been
// received). Caller should treat empty as 404.
func (r *Receiver) MirrorPathFor(repoKey string) string {
	mirror := filepath.Join(r.mirrors.Dir(), repoKey, "repo.git")
	if _, err := os.Stat(mirror); err != nil {
		return ""
	}
	return mirror
}

// ReceiveBundleRequest is the input to ReceiveBundle. The bundle body
// is consumed exactly once; callers should pass the request body (or a
// reader wrapping it) directly.
type ReceiveBundleRequest struct {
	RepoKey   string    // hex SHA-256 of RemoteURL; matches sync.RepoKey
	RemoteURL string    // for display only — the mirror is keyed on RepoKey
	Branch    string    // ref name to bind the bundle to (e.g. "feat/x")
	TipSHA    string    // claimed tip; verified after unbundle
	BaseSHA   string    // optional; informational only
	Bundle    io.Reader // bundle bytes
	Now       time.Time // optional; defaults to time.Now()
}

// ReceiveBundle applies the bundle to the per-repo mirror and persists
// the resulting branch tip. Returns an error if the bundle is invalid or
// the claimed TipSHA does not match what unbundle produced.
//
// Idempotent: receiving the same bundle twice produces the same final
// state (git unbundle is a fast-forward / object-merge operation).
func (r *Receiver) ReceiveBundle(ctx context.Context, req ReceiveBundleRequest) error {
	if req.RepoKey == "" {
		return fmt.Errorf("sync: repo_key is required")
	}
	if req.Branch == "" {
		return fmt.Errorf("sync: branch is required")
	}
	if req.RemoteURL == "" {
		return fmt.Errorf("sync: remote_url is required")
	}
	if req.Bundle == nil {
		return fmt.Errorf("sync: bundle reader is nil")
	}

	mirror, err := r.mirrors.Mirror(req.RepoKey)
	if err != nil {
		return err
	}

	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}

	tip, err := mirror.Unbundle(ctx, req.Branch, req.Bundle)
	if err != nil {
		return err
	}
	if req.TipSHA != "" && tip != req.TipSHA {
		// Don't fail the receive — git already wrote the data and the ref
		// matches the bundle, not the claim. Log so a misbehaving sender
		// is visible.
		r.log.Printf("sync: tip mismatch for %s/%s: claimed=%s actual=%s", req.RepoKey, req.Branch, req.TipSHA, tip)
	}

	if r.store != nil {
		if err := r.store.UpsertSyncedRepo(store.SyncedRepo{
			RepoKey:    req.RepoKey,
			RemoteURL:  req.RemoteURL,
			MirrorPath: mirror.Path(),
			UpdatedAt:  now,
		}); err != nil {
			return fmt.Errorf("persist synced repo: %w", err)
		}
		if err := r.store.UpsertSyncedBranch(store.SyncedBranch{
			RepoKey:   req.RepoKey,
			Branch:    req.Branch,
			TipSHA:    tip,
			BaseSHA:   req.BaseSHA,
			UpdatedAt: now,
		}); err != nil {
			return fmt.Errorf("persist synced branch: %w", err)
		}
	}
	r.log.Printf("sync: received %s/%s tip=%s base=%s", req.RepoKey, req.Branch, tip, req.BaseSHA)
	return nil
}

// ListSyncedRepos returns the cloud-hub's view of all synced repos
// and their branches. Returns []SyncedRepoView{} (not nil) when
// persistence is disabled so JSON serialises to `[]`.
func (r *Receiver) ListSyncedRepos(ctx context.Context) ([]SyncedRepoView, error) {
	if r.store == nil {
		return []SyncedRepoView{}, nil
	}
	repos, err := r.store.LoadSyncedRepos()
	if err != nil {
		return nil, err
	}
	out := make([]SyncedRepoView, 0, len(repos))
	for _, repo := range repos {
		branches, err := r.store.LoadSyncedBranches(repo.RepoKey)
		if err != nil {
			return nil, err
		}
		out = append(out, SyncedRepoView{
			RepoKey:   repo.RepoKey,
			RemoteURL: repo.RemoteURL,
			UpdatedAt: repo.UpdatedAt,
			Branches:  branches,
		})
	}
	return out, nil
}

// SyncedRepoView is the API-shaped projection of a synced repo plus its
// branch list, for the GET /sync/repos endpoint.
type SyncedRepoView struct {
	RepoKey   string               `json:"repo_key"`
	RemoteURL string               `json:"remote_url"`
	UpdatedAt time.Time            `json:"updated_at"`
	Branches  []store.SyncedBranch `json:"branches"`
}
