package sessionsync

import "github.com/acksell/clank/internal/agent"

// Unsynced returns the sessions in current that are absent from the
// last-pushed record rec or that have changed since it (see Changed).
// Shared by `clank status` and `clank push` so both agree on exactly which
// sessions need pushing.
func Unsynced(current []DiscoveredSession, rec agent.SyncedSessionRecord) []DiscoveredSession {
	var out []DiscoveredSession
	for _, s := range current {
		prev, ok := rec.Sessions[s.ExternalID]
		if !ok || Changed(s, prev) {
			out = append(out, s)
		}
	}
	return out
}

// Changed reports whether a discovered session differs from its last-pushed
// record entry. When both sides carry a content fingerprint (Claude's
// last-message uuid) it compares those — immune to the mtime bump a
// read-only `claude --resume` causes, which would otherwise flag a session
// nobody advanced. Otherwise (opencode, whose UpdatedAt is already
// content-based, or a pre-fingerprint record) it falls back to UpdatedAt;
// both sides read it from the same source, so an unchanged session compares
// equal — strict After, never time.Now().
func Changed(cur DiscoveredSession, prev agent.SyncedSession) bool {
	// No recorded content address ⇒ never pushed under the content-addressed
	// scheme; (re)push to mint one. Keeps the rebuilt manifest from ever
	// referencing a session by an empty hash.
	if prev.ContentHash == "" {
		return true
	}
	if cur.Fingerprint != "" && prev.Fingerprint != "" {
		return cur.Fingerprint != prev.Fingerprint
	}
	return cur.UpdatedAt.After(prev.UpdatedAt)
}
