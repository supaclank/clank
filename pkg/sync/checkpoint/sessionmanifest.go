package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/acksell/clank/internal/agent"
)

// SessionManifestVersion is bumped when the on-disk SessionManifest
// schema changes in a non-backwards-compatible way. UnmarshalSessionManifest
// rejects unknown versions.
const SessionManifestVersion = 2

// SessionManifest is the per-checkpoint sidecar describing the opaque
// session export blobs that ride alongside the code bundles in object
// storage. CheckpointID is the foreign key to the code Manifest at the
// same prefix; the pair (Manifest, SessionManifest) is the authoritative
// snapshot of a worktree at a given push/pull.
//
// Session export blobs themselves are opaque to clank — the manifest
// only carries the metadata needed to (a) route the blob to the right
// backend (Backend) on the destination, (b) recreate the host's
// SessionInfo row (Title, Prompt, TicketID, ...), and (c) surface
// per-session warnings to the user (WasBusy).
type SessionManifest struct {
	Version      int            `json:"version"`
	CheckpointID string         `json:"checkpoint_id"`
	Sessions     []SessionEntry `json:"sessions"`
	CreatedAt    time.Time      `json:"created_at"`
	CreatedBy    string         `json:"created_by"`
}

// SessionEntry describes one opencode/claude session captured in a
// checkpoint. ExternalID is the backend-native identity that survives
// import (verified by TestOpenCodeImportSemantics) and is the SOLE id on
// the sync wire. ContentHash is the sha256 of the export blob; the server
// derives the blob's storage key from (ExternalID, ContentHash) via
// storage.KeyForSessionBlob. SessionID is a host-local handle (the host.db
// ULID) carried only so the sprite's import can preserve it — never used
// to address a blob.
type SessionEntry struct {
	SessionID      string              `json:"session_id"`
	ExternalID     string              `json:"external_id"`
	Backend        agent.BackendType   `json:"backend"`
	ContentHash    string              `json:"content_hash"`
	Status         agent.SessionStatus `json:"status"`
	Title          string              `json:"title,omitempty"`
	Prompt         string              `json:"prompt,omitempty"`
	TicketID       string              `json:"ticket_id,omitempty"`
	Agent          string              `json:"agent,omitempty"`
	WorktreeBranch string              `json:"worktree_branch,omitempty"`
	ProjectDir     string              `json:"project_dir,omitempty"`
	CreatedAt      time.Time           `json:"created_at"`
	UpdatedAt      time.Time           `json:"updated_at"`
	Bytes          int64               `json:"bytes"`

	// WasBusy is true if the session was aborted from StatusBusy at the
	// source's quiesce step. Lets the destination surface a "session was
	// interrupted" warning and gives a future auto-resume feature a
	// trigger point. Schema-stable so adding auto-resume later doesn't
	// require a SessionManifestVersion bump.
	WasBusy bool `json:"was_busy,omitempty"`
}

// SessionBlobRef is the minimal address of a content-addressed session
// blob — the backend-native externalID + the sha256 contentHash. The sync
// server derives the storage key from these (storage.KeyForSessionBlob)
// when minting presigned PUT/GET URLs, so the clank SessionID never
// travels the sync wire. Build one from a SessionEntry via BlobRef.
type SessionBlobRef struct {
	ExternalID  string `json:"external_id"`
	ContentHash string `json:"content_hash"`
}

// BlobRef returns the content-addressed blob reference for this entry.
func (e SessionEntry) BlobRef() SessionBlobRef {
	return SessionBlobRef{ExternalID: e.ExternalID, ContentHash: e.ContentHash}
}

// Marshal serializes a SessionManifest to canonical JSON. Mirrors
// Manifest.Marshal; output is deterministic because Go's encoder
// preserves declaration order and SessionEntry contains no maps.
func (m *SessionManifest) Marshal() ([]byte, error) {
	return json.MarshalIndent(m, "", "  ")
}

// ContentDigest is a stable, order-independent sha256 over the
// content-addressed identity of the manifest's sessions — the set of
// (ExternalID, ContentHash) pairs. Two manifests describing the same
// session set (in any order) hash identically; adding, removing, or
// changing a session (which mints a new ContentHash) changes the digest.
//
// Autosync uses it to detect a session-only push: session blobs upload
// straight to object storage with no checkpoint bump or commit callback,
// so the gateway can't be notified — it compares this digest against what
// the sprite last imported (Worktree.SessionsSyncedHash) and re-imports
// only on a change. An empty session set hashes to a fixed constant.
func (m *SessionManifest) ContentDigest() string {
	refs := make([]SessionBlobRef, len(m.Sessions))
	for i, s := range m.Sessions {
		refs[i] = s.BlobRef()
	}
	return ContentDigestForRefs(refs)
}

// ContentDigestForRefs computes the same value as SessionManifest.ContentDigest
// over a bare set of content-addressed refs, without constructing a manifest.
// Lets a caller that holds only the (ExternalID, ContentHash) set — e.g. the
// gateway computing the digest for the sprite's pull-back — produce a digest
// that matches the manifest the client uploads.
func ContentDigestForRefs(refs []SessionBlobRef) string {
	keys := make([]string, len(refs))
	for i, r := range refs {
		keys[i] = r.ExternalID + "\x00" + r.ContentHash
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// UnmarshalSessionManifest parses a SessionManifest blob and rejects
// unknown versions.
func UnmarshalSessionManifest(data []byte) (*SessionManifest, error) {
	var m SessionManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("checkpoint: parse session manifest: %w", err)
	}
	if m.Version != SessionManifestVersion {
		return nil, fmt.Errorf("checkpoint: unsupported session manifest version %d (want %d)", m.Version, SessionManifestVersion)
	}
	return &m, nil
}
