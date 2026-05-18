package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	clanksync "github.com/acksell/clank/pkg/sync"
)

// defaultOwnerCacheTTL is the freshness window for the cached worktree
// ownership view. Reads older than this trigger a refresh against the
// active remote's GET /v1/worktrees on the next lookup. Picked to
// balance two costs: the call rate against the multi-tenant remote
// gateway (sqlite-backed, no per-user machine wake) and the
// user-visible lag after `clank push --migrate` flips ownership before
// the laptop's cloud-icon glyph updates.
const defaultOwnerCacheTTL = 30 * time.Second

// RemoteResolver returns the active remote's base URL and bearer
// token. ok=false when no remote is active (or the active profile is
// missing a gateway_url / access_token) — the cache treats this as
// "no remote-owned worktrees", so all sessions route locally.
type RemoteResolver interface {
	ActiveRemote() (baseURL, jwt string, ok bool)
}

// OwnerCache holds the laptop daemon's in-memory view of per-worktree
// ownership for the active remote. Populated lazily by GET /v1/worktrees
// against the remote gateway, with a TTL refresh.
//
// Concurrency:
//   - Lookup is concurrency-safe; concurrent refreshes dedupe via
//     singleflight.
//   - The whole cache is replaced atomically on refresh; readers
//     either see the old map or the new one, never a partial state.
//
// The cache deliberately holds zero on-disk state. Daemon restart
// re-queries the remote on first lookup; that's the price for not
// needing schema migration / persistence / cache-invalidation
// endpoints in MVP.
type OwnerCache struct {
	resolver   RemoteResolver
	httpClient *http.Client
	ttl        time.Duration

	mu          sync.RWMutex
	entries     map[string]clanksync.OwnerKind // worktree_id → OwnerKind
	refreshedAt time.Time

	sf singleflight.Group
}

// NewOwnerCache constructs an OwnerCache. httpClient is optional;
// nil falls back to an http.Client with a 10s timeout (enough for
// the multi-tenant gateway's sqlite query + transit).
func NewOwnerCache(resolver RemoteResolver, httpClient *http.Client) *OwnerCache {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &OwnerCache{
		resolver:   resolver,
		httpClient: httpClient,
		ttl:        defaultOwnerCacheTTL,
		entries:    map[string]clanksync.OwnerKind{},
	}
}

// SetTTL overrides the refresh interval. Test-only knob — production
// callers stick to the default.
func (c *OwnerCache) SetTTL(ttl time.Duration) {
	c.mu.Lock()
	c.ttl = ttl
	c.mu.Unlock()
}

// Lookup returns the cached owner kind for worktreeID, refreshing
// from the remote if the cache is stale or empty. Returns:
//   - (kind, true, nil) when the remote knows the worktree;
//   - ("", false, nil) when the remote doesn't list it (unknown
//     worktree, or no active remote configured);
//   - ("", false, err) when refresh failed and there's no prior
//     data to fall back on.
//
// When refresh fails but the cache holds prior entries, those are
// served instead — partial liveness beats a hard fail.
func (c *OwnerCache) Lookup(ctx context.Context, worktreeID string) (clanksync.OwnerKind, bool, error) {
	if worktreeID == "" {
		return "", false, fmt.Errorf("owner_cache: empty worktree_id")
	}
	if c.isStale() {
		if err := c.refresh(ctx); err != nil {
			c.mu.RLock()
			empty := len(c.entries) == 0
			c.mu.RUnlock()
			if empty {
				return "", false, err
			}
			// Fall through with stale entries.
		}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	kind, ok := c.entries[worktreeID]
	return kind, ok, nil
}

// Invalidate marks the cache stale so the next Lookup will refresh.
// Useful for tests; production callers rely on the TTL.
func (c *OwnerCache) Invalidate() {
	c.mu.Lock()
	c.refreshedAt = time.Time{}
	c.mu.Unlock()
}

func (c *OwnerCache) isStale() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.refreshedAt.IsZero() || time.Since(c.refreshedAt) > c.ttl
}

// refresh dedupes concurrent calls via singleflight so a TUI startup
// burst (many lookups racing past the TTL boundary) collapses to one
// outbound request.
func (c *OwnerCache) refresh(ctx context.Context) error {
	_, err, _ := c.sf.Do("refresh", func() (any, error) {
		return nil, c.doRefresh(ctx)
	})
	return err
}

func (c *OwnerCache) doRefresh(ctx context.Context) error {
	baseURL, jwt, ok := c.resolver.ActiveRemote()
	if !ok {
		// No remote configured. Treat as "no remote-owned worktrees" —
		// every Lookup returns ok=false, which the routing layer
		// interprets as "stay local". Mark fresh so we don't hammer
		// the resolver every request.
		c.mu.Lock()
		c.entries = map[string]clanksync.OwnerKind{}
		c.refreshedAt = time.Now()
		c.mu.Unlock()
		return nil
	}

	url := strings.TrimRight(baseURL, "/") + "/v1/worktrees"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("owner_cache: build request: %w", err)
	}
	if jwt != "" {
		req.Header.Set("Authorization", "Bearer "+jwt)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("owner_cache: GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("owner_cache: GET %s: HTTP %d", url, resp.StatusCode)
	}

	var body struct {
		Worktrees []struct {
			ID        string              `json:"id"`
			OwnerKind clanksync.OwnerKind `json:"owner_kind"`
		} `json:"worktrees"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fmt.Errorf("owner_cache: decode %s: %w", url, err)
	}

	entries := make(map[string]clanksync.OwnerKind, len(body.Worktrees))
	for _, wt := range body.Worktrees {
		entries[wt.ID] = wt.OwnerKind
	}
	c.mu.Lock()
	c.entries = entries
	c.refreshedAt = time.Now()
	c.mu.Unlock()
	return nil
}
