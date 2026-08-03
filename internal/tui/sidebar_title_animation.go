package tui

import "github.com/supaclank/clank/internal/agent"

// Typewriter animation for session titles in the sidebar.
//
// When a session's Title changes (most commonly empty → AI-generated),
// the sidebar reveals the new value one chunk at a time instead of
// swapping it in a single frame. Without this, the user perceives the
// old title as "disappearing" — especially jarring when the prior
// row was identified by its Prompt and the new Title looks unrelated.
//
// The animation is driven by the same spinner tick that already feeds
// SetCloudSpinnerFrame, so we don't introduce a second ticker. The
// sidebar exposes AdvanceTitleAnimations() to step every active
// animation by titleAnimationCharsPerTick runes; rendering uses
// renderedTitleFor() to pull the partially-revealed string.

// titleAnimationCharsPerTick is the number of runes revealed per
// spinner tick. The default spinner runs at ~12.5 Hz (80ms) so 2
// chars/tick yields ~25 chars/sec — close to a fast human typist
// without feeling sluggish on long titles.
const titleAnimationCharsPerTick = 2

// titleAnimation tracks one in-flight typewriter for a session row.
// runes holds the target title pre-split so per-tick advancement is
// O(1). Revealed counts how many leading runes are currently visible.
type titleAnimation struct {
	runes    []rune
	revealed int
}

// done reports whether the full target title has been revealed.
func (a *titleAnimation) done() bool {
	return a.revealed >= len(a.runes)
}

// prefix returns the currently-revealed substring of the target title.
func (a *titleAnimation) prefix() string {
	if a.revealed >= len(a.runes) {
		return string(a.runes)
	}
	return string(a.runes[:a.revealed])
}

// ensureTitleAnimMap lazily allocates the animation map; callers
// outside this file must go through startTitleAnimation so the map
// is initialized before access.
func (m *SidebarModel) ensureTitleAnimMap() {
	if m.titleAnimations == nil {
		m.titleAnimations = map[string]*titleAnimation{}
	}
	if m.lastTitleBySession == nil {
		m.lastTitleBySession = map[string]string{}
	}
}

// startTitleAnimation begins a typewriter for sessionID targeting
// title. Called from SetSessions when a row's Title changes between
// snapshots. The target is flattened to one line so a revealed prefix
// can't carry a newline mid-animation (which would push the row past its
// budgeted two lines). A zero-rune title is a no-op (nothing to animate).
func (m *SidebarModel) startTitleAnimation(sessionID, title string) {
	m.ensureTitleAnimMap()
	runes := []rune(singleLine(title))
	if len(runes) == 0 {
		delete(m.titleAnimations, sessionID)
		return
	}
	m.titleAnimations[sessionID] = &titleAnimation{runes: runes}
}

// hasActiveTitleAnimation reports whether sessionID is currently
// mid-typewriter. Used by tests and (indirectly) by renderSessionRow
// via renderedTitleFor.
func (m *SidebarModel) hasActiveTitleAnimation(sessionID string) bool {
	a, ok := m.titleAnimations[sessionID]
	if !ok {
		return false
	}
	return !a.done()
}

// renderedTitleFor returns the title text the sidebar should paint
// for sessionID this frame. While an animation is in flight it
// returns the revealed prefix; otherwise it returns full unchanged.
// Callers still apply truncation and styling to the returned value.
func (m *SidebarModel) renderedTitleFor(sessionID, full string) string {
	a, ok := m.titleAnimations[sessionID]
	if !ok || a.done() {
		return full
	}
	return a.prefix()
}

// AdvanceTitleAnimations steps every active typewriter forward by
// titleAnimationCharsPerTick runes. Completed animations are dropped
// so subsequent frames render the full title without the cursor glyph.
// Cheap to call every spinner tick even when nothing is animating.
func (m *SidebarModel) AdvanceTitleAnimations() {
	if len(m.titleAnimations) == 0 {
		return
	}
	for id, a := range m.titleAnimations {
		a.revealed += titleAnimationCharsPerTick
		if a.done() {
			delete(m.titleAnimations, id)
		}
	}
}

// diffTitlesForAnimation compares the incoming session snapshot
// against the previous snapshot recorded in lastTitleBySession.
// For each session whose Title is non-empty and differs from the
// recorded value AND was previously recorded (i.e. not a brand-new
// row from first load), a typewriter is started. The recorded map
// is updated in lockstep so the next call diffs against the latest
// known state.
//
// First-load behavior: when lastTitleBySession is empty we only
// record the snapshot — animating every pre-existing title at
// startup would flood the sidebar with motion and obscure the new-
// title signal we actually care about.
func (m *SidebarModel) diffTitlesForAnimation(sessions []agent.SessionInfo) {
	m.ensureTitleAnimMap()
	firstLoad := len(m.lastTitleBySession) == 0

	seen := make(map[string]struct{}, len(sessions))
	for _, s := range sessions {
		seen[s.ID] = struct{}{}
		prev, known := m.lastTitleBySession[s.ID]
		m.lastTitleBySession[s.ID] = s.Title

		if firstLoad || !known {
			continue
		}
		// Title cleared back to "" — drop any in-flight animation so the
		// row doesn't keep rendering the stale prefix.
		if s.Title == "" {
			delete(m.titleAnimations, s.ID)
			continue
		}
		if s.Title == prev {
			continue
		}
		m.startTitleAnimation(s.ID, s.Title)
	}

	// Drop entries for sessions that disappeared so the map can't
	// grow unbounded across long-lived TUI runs.
	for id := range m.lastTitleBySession {
		if _, ok := seen[id]; !ok {
			delete(m.lastTitleBySession, id)
			delete(m.titleAnimations, id)
		}
	}
}
