// Package host defines the value types for Clank's Host plane and the
// host.Service that runs agent backends, owns clones of remote repos,
// and exposes an HTTP API to the Hub.
package host

import (
	"time"

	"github.com/acksell/clank/internal/agent"
)

// Hostname identifies a Host within the Hub's catalog. It is a short,
// human-readable slug ("local", "daytona-abc123") chosen by whoever
// registers the Host. The Hub treats it as an opaque key.
type Hostname string

const (
	// HostLocal is the canonical ID for the laptop's supervised clank-host
	// child. The TUI defaults to this until the host-selection UX lands.
	HostLocal Hostname = "local"
)

// BackendInfo describes one backend installed on a Host (e.g. "opencode",
// "claude-code"). Catalog endpoints return slices of these.
type BackendInfo struct {
	Name        agent.BackendType `json:"name"`
	DisplayName string            `json:"display_name"`
	Available   bool              `json:"available"`         // false when the backend's binary/server is missing
	Reason      string            `json:"reason,omitempty"`  // Why it is unavailable (when Available is false)
	Version     string            `json:"version,omitempty"` // Reported by the backend, when available
}

// AgentInfo is re-exported from the agent package so callers in the host
// plane import a single source.
type AgentInfo = agent.AgentInfo

// ModelInfo is re-exported from the agent package for the same reason as
// AgentInfo.
type ModelInfo = agent.ModelInfo

// BranchInfo describes a branch on a Host's repo.
type BranchInfo struct {
	Name         string `json:"name"`
	WorktreeDir  string `json:"worktree_dir,omitempty"` // Filesystem path on the Host if a worktree is checked out
	IsDefault    bool   `json:"is_default,omitempty"`
	IsCurrent    bool   `json:"is_current,omitempty"`
	LinesAdded   int    `json:"lines_added,omitempty"`
	LinesRemoved int    `json:"lines_removed,omitempty"`
	CommitsAhead int    `json:"commits_ahead,omitempty"`
	// RepoLabel is the display name used to group branches in the sidebar.
	// Set to the remote name when available; falls back to the basename of
	// the repo root so it is always non-empty and unique for local repos.
	RepoLabel string `json:"repo_label,omitempty"`
}

// WorktreeInfo describes a single worktree managed by the Host.
type WorktreeInfo struct {
	Branch      string `json:"branch"`
	WorktreeDir string `json:"worktree_dir"`
}

// HostStatus is the response body of `GET /status` on the Host API. Hub
// surfaces a derived view (online/offline + last seen) to clients.
type HostStatus struct {
	Hostname  Hostname  `json:"hostname"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"started_at"`
	Sessions  int       `json:"sessions"` // Number of live (backend-attached) sessions
}
