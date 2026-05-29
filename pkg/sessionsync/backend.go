// Package sessionsync is a daemon-free, backend-agnostic view of AI
// coding-agent sessions: discovery (list), export, and import, reading
// each backend's own local storage directly. It is the layer `clank
// push` uses to mirror sessions to a remote without a running
// clank-host or backend server.
//
// It deliberately does NOT manage session lifecycle (start/stop/stream)
// — that's internal/agent's BackendManager/SessionBackend. This layer
// is the lightweight transfer surface only.
package sessionsync

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/acksell/clank/internal/agent"
)

// ErrExportNotImplemented is returned by a Backend whose export/import
// transfer isn't built yet (currently Claude Code — discovery only).
var ErrExportNotImplemented = errors.New("sessionsync: export/import not implemented for this backend")

// DiscoveredSession is the daemon-free view of a backend session —
// thinner than agent.SessionInfo (no host.db ULID, no status machine).
// ExternalID is the identity end-to-end: with no local metadata store
// to mint a clank ULID, the backend-native session id is what both the
// laptop and the remote refer to.
type DiscoveredSession struct {
	Backend    agent.BackendType
	ExternalID string
	Title      string
	ProjectDir string // opencode "directory" / Claude Cwd
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Backend is a daemon-free session source for one agent backend.
// Implementations read the backend's own on-disk storage (or its
// hermetic CLI) — no running clank-host or backend server required.
//
// Concurrency: methods may be called concurrently; implementations are
// stateless subprocess/SDK wrappers.
type Backend interface {
	Type() agent.BackendType

	// ListSessions enumerates sessions visible from projectDir. An empty
	// projectDir means "all sessions" where the backend supports it;
	// opencode requires a directory to scope to a worktree.
	ListSessions(ctx context.Context, projectDir string) ([]DiscoveredSession, error)

	// ExportSession writes the backend's export blob for externalID to dst.
	ExportSession(ctx context.Context, projectDir, externalID string, dst io.Writer) error

	// ImportSession installs an export blob and returns the backend-native
	// session id it was filed under.
	ImportSession(ctx context.Context, projectDir, blobPath string) (externalID string, err error)
}

// millisToTime converts opencode/Claude epoch-millisecond timestamps to
// time.Time. A zero input maps to the zero time.
func millisToTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}
