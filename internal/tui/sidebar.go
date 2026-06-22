package tui

// SidebarModel is the IDE-style navigation sidebar of the inbox layout.
//
// The visible body is a flat list of sidebarNode rows produced by
// flattenSidebar(tree, expanded). The tree itself is rebuilt from the
// session list on each SetSessions; expand state survives rebuilds
// because it keys on stable node Keys (LocalPath, etc.).
//
// Layout (top-to-bottom):
//
//	[0]             → AllSessions (virtual; selecting opens the inbox)
//	[1 .. N]        → Recent worktrees, each expandable into sessions
//	[…]             → Older worktrees bucket (collapsible)
//	[footer]        → Import / Cloud / Settings rows pinned to bottom
//
// Cursor model: linear `cursor int` indexing into the flattened node
// list. Section breakpoints (used by shift+up/down) are derived from
// node Kinds, so adding rows never requires renumbering.

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/acksell/clank/internal/agent"
	daemonclient "github.com/acksell/clank/internal/daemonclient"
	"github.com/acksell/clank/internal/host"
)

// sidebarWidth is the fallback width of the sidebar (including border)
// used when the screen width is not yet known.
const sidebarWidth = 30

// composeRequestedMsg is emitted when the user presses "n" (or Shift+N) in
// the sidebar. The inbox handles it by opening the compose view, prefilled
// with WorktreePath (or the cwd when empty). Shift+N additionally opens
// compose with the New-worktree toggle pre-enabled.
type composeRequestedMsg struct {
	worktreePath string
	newWorktree  bool // true when opened via Shift+N (compose on a fresh worktree)
}

// SettingsRequestedMsg is emitted by the inbox when the user activates the
// "⚙ Settings" footer entry in the sidebar.
type SettingsRequestedMsg struct{}

// ImportSessionsRequestedMsg is emitted when the user activates the
// "↓ Import Sessions" footer entry in the sidebar.
type ImportSessionsRequestedMsg struct{}

// worktreeOptionsRequestedMsg is emitted when the user presses ':' on a
// worktree entry. The inbox opens a Push/Pull action menu for that path.
type worktreeOptionsRequestedMsg struct {
	localPath string
}

// sessionSelectedFromSidebarMsg is emitted when the user presses Enter
// on a session node in the sidebar tree. The inbox handles it by opening
// the session view for that ID.
type sessionSelectedFromSidebarMsg struct {
	sessionID string
}

// sidebarExpandToggledMsg is emitted after an expand/collapse so the
// inbox can persist the new state to preferences.
type sidebarExpandToggledMsg struct{}

// SidebarModel renders the sidebar tree and tracks cursor/expand state.
type SidebarModel struct {
	client     *daemonclient.Client
	projectDir string
	hostname   string
	gitRef     agent.GitRef

	sessions        []agent.SessionInfo // cached so toggling expand can rebuild without re-fetch
	tree            sidebarTree
	flat            []sidebarNode
	expanded        map[string]bool  // effective expand state: defaults + user overrides
	userToggles     map[string]bool  // explicit user expand/collapse choices (persisted)
	cycleIdx        map[string]int   // per-worktree index used by Shift+Enter session-rotate
	activeSessionID string           // session currently rendered in the right pane (drives the left-rail indicator)
	nowFn           func() time.Time // injected for deterministic tests

	cursor int
	scroll int

	// rowFlat maps each rendered content line (sidebar-local, before the
	// top border) to the flat node index drawn there, or -1 for the
	// header / blanks / padding. Rebuilt every View() so a mouse click
	// can resolve the row under the cursor. NodeAtRow reads it.
	rowFlat []int

	focused bool
	width   int
	height  int
	err     error

	// cloudStatus is mirrored from the inbox so the "☁ Cloud" footer
	// row can render a connection indicator. Defaults to
	// cloudStatusNotConfigured (zero value) until SetCloudStatus is called.
	cloudStatus       cloudAuthStatus
	cloudSpinnerFrame string

	// pendingPushes is mirrored from the inbox so worktree rows can
	// paint an animated spinner next to whichever LocalPath has a
	// push request in flight. spinnerFrame holds the current spinner
	// glyph (kept in sync via SetSpinnerFrame on every tick).
	pendingPushes map[string]bool
	spinnerFrame  string

	// titleAnimations holds in-flight typewriter state keyed by
	// session ID; see sidebar_title_animation.go.
	titleAnimations    map[string]*titleAnimation
	lastTitleBySession map[string]string
}

// NewSidebarModel creates a sidebar for the given repo identity.
// projectDir is retained for display purposes only.
func NewSidebarModel(client *daemonclient.Client, hostname string, gitRef agent.GitRef, projectDir string) SidebarModel {
	return SidebarModel{
		client:      client,
		hostname:    hostname,
		gitRef:      gitRef,
		projectDir:  projectDir,
		expanded:    map[string]bool{},
		userToggles: map[string]bool{},
		cycleIdx:    map[string]int{},
		nowFn:       time.Now,
		cursor:      0, // AllSessions selected by default
	}
}

// Init is a no-op; the sidebar is populated via SetSessions.
func (m *SidebarModel) Init() tea.Cmd { return nil }

// SetExpanded seeds the persisted user-toggle map (e.g. from
// Preferences.SidebarExpanded). Older buckets always start collapsed
// regardless of what was persisted — see sanitizeExpanded for the
// reset rules. Auto-defaults (visible worktrees auto-expand) are
// applied during rebuildFlat, not stored here. Safe to call before
// SetSessions.
func (m *SidebarModel) SetExpanded(seed map[string]bool) {
	if seed == nil {
		m.userToggles = map[string]bool{}
	} else {
		m.userToggles = sanitizeExpanded(seed)
	}
	m.rebuildFlat()
}

// SnapshotExpanded returns a shallow copy of the explicit user toggle
// map. Auto-defaults (visible worktrees) are deliberately NOT included
// — they're computed every launch, persisting them would make a future
// default change invisible to existing users.
func (m *SidebarModel) SnapshotExpanded() map[string]bool {
	out := make(map[string]bool, len(m.userToggles))
	for k, v := range m.userToggles {
		if isOlderBucketKey(k) {
			continue
		}
		out[k] = v
	}
	return out
}

// computeEffectiveExpanded combines auto-defaults (only the cwd's
// worktree expands on first sight) with the persisted user toggles.
// User choices win over defaults so an explicit collapse stays
// collapsed across rebuilds — and any other worktree the user
// expanded once stays expanded.
//
// The narrow auto-expand rule (cwd only) keeps the sidebar quiet at
// startup. The cwd is where the user is right now, so showing its
// sessions inline is the only default that earns its vertical
// space.
func (m *SidebarModel) computeEffectiveExpanded() map[string]bool {
	out := make(map[string]bool, 1+len(m.userToggles))
	if m.gitRef.LocalPath != "" {
		out["wt:"+m.gitRef.LocalPath] = true
	}
	for k, v := range m.userToggles {
		out[k] = v
	}
	return out
}

// SetSessions rebuilds the tree from the provided sessions. Cached so
// toggle operations can re-flatten without re-fetching.
func (m *SidebarModel) SetSessions(sessions []agent.SessionInfo) {
	m.diffTitlesForAnimation(sessions)
	m.sessions = sessions
	m.rebuildTree()
}

// rebuildTree rebuilds tree + flat list from m.sessions. Called whenever
// the source data or expand state changes.
func (m *SidebarModel) rebuildTree() {
	m.tree = buildSidebarTree(m.sessions, m.gitRef.LocalPath, m.nowFn())
	m.rebuildFlat()
	m.pruneStaleExpandedKeys()
	m.clampCursor()
}

// rebuildFlat re-flattens the current tree with the current effective
// expand state (auto-defaults + user toggles). Separated from
// rebuildTree so SetExpanded can update visibility without re-bucketing
// sessions.
func (m *SidebarModel) rebuildFlat() {
	m.expanded = m.computeEffectiveExpanded()
	m.flat = flattenSidebar(m.tree, m.expanded, m.nowFn())
	m.clampCursor()
}

// pruneStaleExpandedKeys deletes any "wt:" or "older:s:" keys whose
// worktrees no longer appear in the tree, so the persisted map can't
// grow unbounded across years of use.
func (m *SidebarModel) pruneStaleExpandedKeys() {
	alive := make(map[string]bool, len(m.tree.RecentWorktrees)+len(m.tree.OlderWorktrees.Hidden))
	for _, w := range m.tree.RecentWorktrees {
		alive["wt:"+w.LocalPath] = true
		alive["older:s:"+w.LocalPath] = true
	}
	for _, w := range m.tree.OlderWorktrees.Hidden {
		alive["wt:"+w.LocalPath] = true
		alive["older:s:"+w.LocalPath] = true
	}
	alive["older:wt"] = true
	for k := range m.userToggles {
		if strings.HasPrefix(k, "wt:") || strings.HasPrefix(k, "older:") {
			if !alive[k] {
				delete(m.userToggles, k)
			}
		}
	}
}

// clampCursor keeps the cursor inside the flat list and resets scroll
// when the list shrinks below the previous offset.
func (m *SidebarModel) clampCursor() {
	if last := len(m.flat) - 1; last < 0 {
		m.cursor = 0
		m.scroll = 0
		return
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor > len(m.flat)-1 {
		m.cursor = len(m.flat) - 1
	}
	if m.scroll < 0 {
		m.scroll = 0
	}
}

// toggleExpand flips the expand state for the node at the cursor when
// it's expandable. The flip is written to userToggles (the persistable
// map), then rebuildFlat recomputes the effective state. Returns true
// when the state actually changed so the caller can emit a persistence
// message.
func (m *SidebarModel) toggleExpand() bool {
	if m.cursor < 0 || m.cursor >= len(m.flat) {
		return false
	}
	n := m.flat[m.cursor]
	if !n.IsExpandable() {
		return false
	}
	m.userToggles[n.Key()] = !m.expanded[n.Key()]
	m.rebuildFlat()
	return true
}

// SetActiveSessionID stamps the session currently rendered in the
// right pane. The sidebar uses it to paint a left-edge rail next to
// that session's row, so the user can see "what's open" independent of
// where their arrow-key cursor sits. Pass "" to clear.
func (m *SidebarModel) SetActiveSessionID(id string) { m.activeSessionID = id }

// SetCloudStatus updates the cloud connection indicator shown next to
// the "☁ Cloud" footer row.
func (m *SidebarModel) SetCloudStatus(s cloudAuthStatus) { m.cloudStatus = s }

// SetCloudSpinnerFrame feeds the current spinner glyph from the inbox.
func (m *SidebarModel) SetCloudSpinnerFrame(frame string) {
	m.cloudSpinnerFrame = frame
	m.spinnerFrame = frame // any animated indicator can share the same frame
}

// SetPendingPushes mirrors the inbox's map of worktree-LocalPath →
// in-flight push. Worktree rows whose path is present render an
// animated spinner alongside the label until the result clears the
// entry. The map is read-only from the sidebar's perspective; the
// inbox owns the lifecycle.
func (m *SidebarModel) SetPendingPushes(pushes map[string]bool) {
	m.pendingPushes = pushes
}

// CursorOnImport reports whether the cursor is on the import row.
func (m *SidebarModel) CursorOnImport() bool {
	return m.cursorNodeKind() == nodeImport
}

// CursorOnCloud reports whether the cursor is on the cloud row.
func (m *SidebarModel) CursorOnCloud() bool {
	return m.cursorNodeKind() == nodeCloud
}

// CursorOnSettings reports whether the cursor is on the settings row.
func (m *SidebarModel) CursorOnSettings() bool {
	return m.cursorNodeKind() == nodeSettings
}

// CursorOnAllSessions reports whether the cursor is on the "All sessions"
// row (which surfaces the date-grouped inbox in the right pane).
func (m *SidebarModel) CursorOnAllSessions() bool {
	return m.cursorNodeKind() == nodeAllSessions
}

// SetCursor moves the cursor to the given flat index, clamped to the
// list. Used by mouse-click selection to jump straight to a row.
func (m *SidebarModel) SetCursor(idx int) {
	m.cursor = idx
	m.clampCursor()
}

// cursorNodeKind returns the kind of the node under the cursor, or -1
// when the flat list is empty.
func (m *SidebarModel) cursorNodeKind() sidebarNodeKind {
	if m.cursor < 0 || m.cursor >= len(m.flat) {
		return -1
	}
	return m.flat[m.cursor].Kind()
}

// SelectedBranch returns the LocalPath used by the inbox to filter its
// session list. In the IDE-style sidebar the inbox is only ever active
// on the AllSessions row, so this always returns "" — preserved for
// call-site compatibility (the badges in inbox.renderRow read it).
func (m *SidebarModel) SelectedBranch() string { return "" }

// SelectedWorktreeDir mirrors SelectedBranch — always "" in the
// tree-driven sidebar. Kept for call-site compatibility.
func (m *SidebarModel) SelectedWorktreeDir() string { return "" }

// SelectedBranchInfo always returns nil; merge overlay disabled until
// sessions carry git branch metadata.
func (m *SidebarModel) SelectedBranchInfo() *host.BranchInfo { return nil }

// SelectedSessionID returns the session id under the cursor, or "" when
// the cursor isn't on a session row.
func (m *SidebarModel) SelectedSessionID() string {
	if m.cursor < 0 || m.cursor >= len(m.flat) {
		return ""
	}
	n, ok := m.flat[m.cursor].(sessionNode)
	if !ok {
		return ""
	}
	return n.Session.ID
}

// CursorWorktreePath returns the LocalPath of the worktree the cursor
// is currently on (or whose session the cursor is on). Empty means
// the cursor isn't anywhere worktree-shaped (AllSessions, Older
// buckets, footer rows). Callers fall back to the cwd's worktree in
// that case.
//
// Used by the unified "n" gesture to prefill the compose view's
// target worktree from context.
func (m *SidebarModel) CursorWorktreePath() string {
	if m.cursor < 0 || m.cursor >= len(m.flat) {
		return ""
	}
	switch n := m.flat[m.cursor].(type) {
	case worktreeNode:
		return n.LocalPath
	case sessionNode:
		return n.ParentPath
	}
	return ""
}

// SetFocused sets whether the sidebar has keyboard focus.
func (m *SidebarModel) SetFocused(focused bool) { m.focused = focused }

// Focused reports whether the sidebar has keyboard focus.
func (m *SidebarModel) Focused() bool { return m.focused }

// SetSize sets the sidebar dimensions.
func (m *SidebarModel) SetSize(width, height int) {
	m.width = width
	m.height = height
}

// Update handles messages for the sidebar.
func (m *SidebarModel) Update(msg tea.Msg) tea.Cmd {
	if keyMsg, ok := msg.(tea.KeyPressMsg); ok {
		return m.handleKey(keyMsg)
	}
	return nil
}

// shiftJump moves the cursor one worktree in the chosen direction,
// skipping over any session rows in between so the gesture stays
// coarse and symmetric (shift+down then shift+up returns to where you
// started). When there's no worktree left in that direction it falls
// back to the next section anchor — AllSessions / Older bucket /
// footer — so the user can still navigate to the edges of the list.
func (m *SidebarModel) shiftJump(forward bool) {
	if len(m.flat) == 0 {
		return
	}
	delta, end := 1, len(m.flat)
	if !forward {
		delta, end = -1, -1
	}

	matchers := []func(int) bool{
		m.isWorktreeAt,
		m.isSectionAnchorAt,
	}
	for _, match := range matchers {
		for i := m.cursor + delta; i != end; i += delta {
			if match(i) {
				m.cursor = i
				return
			}
		}
	}
}

// isWorktreeAt reports whether the row at i is any worktree row
// (expanded or collapsed).
func (m *SidebarModel) isWorktreeAt(i int) bool {
	return m.flat[i].Kind() == nodeWorktree
}

// isSectionAnchorAt returns true for the rows shift+up/down can fall
// back to when there's no worktree to jump to: AllSessions, the Older
// bucket, and the footer entries.
func (m *SidebarModel) isSectionAnchorAt(i int) bool {
	switch m.flat[i].Kind() {
	case nodeAllSessions, nodeOlderWorktrees, nodeImport, nodeCloud, nodeSettings:
		return true
	}
	return false
}

// isOlderBucketKey reports whether the supplied expand-state key
// addresses one of the auto-collapsed Older buckets.
func isOlderBucketKey(k string) bool {
	return k == "older:wt" || strings.HasPrefix(k, "older:s:")
}

// sanitizeExpanded returns a fresh map matching seed but with any
// older-bucket keys removed — those reset to collapsed at every launch.
func sanitizeExpanded(seed map[string]bool) map[string]bool {
	out := make(map[string]bool, len(seed))
	for k, v := range seed {
		if isOlderBucketKey(k) {
			continue
		}
		out[k] = v
	}
	return out
}
