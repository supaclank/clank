package hub

// Backwards-compat aliases for worktree wire types whose canonical home moved
// to internal/host (Phase 2A of hub_host_refactor.md). New code should import
// from internal/host directly; these aliases let in-tree daemon callers and
// tests keep their existing references during the migration.

import "github.com/acksell/clank/internal/host"

type (
	BranchInfo   = host.BranchInfo
	WorktreeInfo = host.WorktreeInfo
)
