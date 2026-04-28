package host

import (
	"sync"
	"time"
)

// DefaultBranchCacheTTL is the default time-to-live for cached
// listBranches results. The TUI inbox polls listBranches every 3s on
// each open session worktree; without caching, every poll fans out to
// ~4 git subprocesses per active worktree (DiffStat = 3, CommitsAhead
// = 1). DiffStat in particular runs `git diff --numstat HEAD` which
// stat()s the working tree. With 2 active sessions on a busy repo this
// pegged clank-host's CPU. The TTL is short enough that branch
// metadata still feels live (a few seconds of staleness is invisible
// in the inbox row "+12 -3" indicators) while collapsing fork rate.
const DefaultBranchCacheTTL = 5 * time.Second

// branchCache is a per-projectDir TTL cache for listBranches results.
// Concurrent ListBranches calls for the same projectDir share the
// cached slice. The slice is treated as immutable after caching.
type branchCache struct {
	ttl   time.Duration
	now   func() time.Time
	mu    sync.Mutex
	items map[string]branchCacheEntry
}

type branchCacheEntry struct {
	info     []BranchInfo
	cachedAt time.Time
}

func newBranchCache(ttl time.Duration, now func() time.Time) *branchCache {
	if ttl <= 0 {
		ttl = DefaultBranchCacheTTL
	}
	if now == nil {
		now = time.Now
	}
	return &branchCache{
		ttl:   ttl,
		now:   now,
		items: make(map[string]branchCacheEntry),
	}
}

// get returns the cached branches for projectDir if present and fresh.
func (c *branchCache) get(projectDir string) ([]BranchInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[projectDir]
	if !ok {
		return nil, false
	}
	if c.now().Sub(e.cachedAt) >= c.ttl {
		return nil, false
	}
	return e.info, true
}

// put stores branches for projectDir.
func (c *branchCache) put(projectDir string, info []BranchInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[projectDir] = branchCacheEntry{
		info:     info,
		cachedAt: c.now(),
	}
}

// invalidate drops any cached entry for projectDir. Call when an
// operation on this host is known to have changed branch state
// (worktree create/remove, merge).
func (c *branchCache) invalidate(projectDir string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, projectDir)
}
