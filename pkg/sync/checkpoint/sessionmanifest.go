package checkpoint

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/acksell/clank/internal/agent"
)

// SessionManifestVersion is bumped when the on-disk SessionManifest
// schema changes in a non-backwards-compatible way. UnmarshalSessionManifest
// rejects unknown versions.
const SessionManifestVersion = 1

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
// checkpoint. SessionID is the cross-machine stable identity (the
// host-side ULID); ExternalID is the backend's native identifier and
// survives `opencode import` (verified by TestOpenCodeImportSemantics).
// BlobKey is the path component appended to the checkpoint's S3 prefix
// to address this session's exported blob.
type SessionEntry struct {
	SessionID      string              `json:"session_id"`
	ExternalID     string              `json:"external_id"`
	Backend        agent.BackendType   `json:"backend"`
	BlobKey        string              `json:"blob_key"`
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

// Marshal serializes a SessionManifest to canonical JSON. Mirrors
// Manifest.Marshal; output is deterministic because Go's encoder
// preserves declaration order and SessionEntry contains no maps.
func (m *SessionManifest) Marshal() ([]byte, error) {
	return json.MarshalIndent(m, "", "  ")
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
