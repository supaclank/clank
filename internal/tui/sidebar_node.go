package tui

import (
	"time"

	"github.com/acksell/clank/internal/agent"
)

// sidebarNodeKind discriminates the concrete kind of a sidebar node.
// It exists so render/key-dispatch code can switch on a single value
// instead of doing type assertions for every row.
type sidebarNodeKind int

const (
	nodeAllSessions sidebarNodeKind = iota
	nodeWorktree
	nodeSession
	nodeOlderWorktrees
	nodeOlderSessions
	nodeImport
	nodeCloud
	nodeSettings
)

// sidebarNode is one row in the flattened sidebar tree. Implementations
// describe how the row participates in navigation (selectable, expandable)
// and how it identifies itself across rebuilds (Key) so expand-state
// survives session reshuffles.
type sidebarNode interface {
	Kind() sidebarNodeKind
	// Key is a stable identifier used as a map key in the expand-state
	// map. Must be unique across the whole tree.
	Key() string
	IsSelectable() bool
	IsExpandable() bool
	// Depth is the nesting level used for indentation. 0 = top-level row.
	Depth() int
}

// allSessionsNode is the virtual "All sessions" entry pinned at the top
// of the sidebar. Selecting it surfaces the date-grouped inbox.
type allSessionsNode struct{}

func (allSessionsNode) Kind() sidebarNodeKind { return nodeAllSessions }
func (allSessionsNode) Key() string           { return "all" }
func (allSessionsNode) IsSelectable() bool    { return true }
func (allSessionsNode) IsExpandable() bool    { return false }
func (allSessionsNode) Depth() int            { return 0 }

// worktreeNode represents one worktree row. It carries the session set
// used to render the child rows when expanded; PartitionSessionsByAge
// further splits Sessions into a visible-recent slice and a collapsed
// olderSessionsNode.
type worktreeNode struct {
	LocalPath       string
	Label           string
	RepoLabel       string
	LatestUpdatedAt time.Time
	WorktreeID      string
	Sessions        []agent.SessionInfo
	// Total/Active/Done/Archived mirror worktreeEntry so the new tree
	// model can render the same count badges as the legacy sidebar.
	Total    int
	Active   int
	Done     int
	Archived int
}

func (worktreeNode) Kind() sidebarNodeKind { return nodeWorktree }
func (n worktreeNode) Key() string         { return "wt:" + n.LocalPath }
func (worktreeNode) IsSelectable() bool    { return true }
func (worktreeNode) IsExpandable() bool    { return true }
func (worktreeNode) Depth() int            { return 0 }

// sessionNode is a single session row living under an expanded worktree.
type sessionNode struct {
	Session    agent.SessionInfo
	ParentPath string
}

func (sessionNode) Kind() sidebarNodeKind { return nodeSession }
func (n sessionNode) Key() string         { return "s:" + n.Session.ID }
func (sessionNode) IsSelectable() bool    { return true }
func (sessionNode) IsExpandable() bool    { return false }
func (sessionNode) Depth() int            { return 1 }

// olderWorktreesNode is the collapsible "Older" bucket pinned below the
// recent worktrees. Hidden carries the worktrees that fall past the
// OlderCutoff so the renderer can flatten them when the bucket expands.
type olderWorktreesNode struct {
	Hidden []worktreeNode
}

func (olderWorktreesNode) Kind() sidebarNodeKind { return nodeOlderWorktrees }
func (olderWorktreesNode) Key() string           { return "older:wt" }
func (olderWorktreesNode) IsSelectable() bool    { return true }
func (olderWorktreesNode) IsExpandable() bool    { return true }
func (olderWorktreesNode) Depth() int            { return 0 }

// olderSessionsNode is the per-worktree collapsible bucket holding
// sessions older than OlderCutoff. ParentPath is the LocalPath of the
// owning worktreeNode so the Key disambiguates between buckets.
type olderSessionsNode struct {
	ParentPath string
	Hidden     []agent.SessionInfo
}

func (olderSessionsNode) Kind() sidebarNodeKind { return nodeOlderSessions }
func (n olderSessionsNode) Key() string         { return "older:s:" + n.ParentPath }
func (olderSessionsNode) IsSelectable() bool    { return true }
func (olderSessionsNode) IsExpandable() bool    { return true }
func (olderSessionsNode) Depth() int            { return 1 }

// importNode, cloudNode, settingsNode are the three footer entries
// pinned at the bottom of the sidebar.
type importNode struct{}

func (importNode) Kind() sidebarNodeKind { return nodeImport }
func (importNode) Key() string           { return "footer:import" }
func (importNode) IsSelectable() bool    { return true }
func (importNode) IsExpandable() bool    { return false }
func (importNode) Depth() int            { return 0 }

type cloudNode struct{}

func (cloudNode) Kind() sidebarNodeKind { return nodeCloud }
func (cloudNode) Key() string           { return "footer:cloud" }
func (cloudNode) IsSelectable() bool    { return true }
func (cloudNode) IsExpandable() bool    { return false }
func (cloudNode) Depth() int            { return 0 }

type settingsNode struct{}

func (settingsNode) Kind() sidebarNodeKind { return nodeSettings }
func (settingsNode) Key() string           { return "footer:settings" }
func (settingsNode) IsSelectable() bool    { return true }
func (settingsNode) IsExpandable() bool    { return false }
func (settingsNode) Depth() int            { return 0 }
