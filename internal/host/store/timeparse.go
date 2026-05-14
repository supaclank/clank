package store

import (
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// parseLegacyTimeMs converts a stored time string from the pre-v3
// DATETIME-as-TEXT schema into unix milliseconds. Used during the v3
// migration to backfill the new INTEGER columns.
//
// modernc.org/sqlite v1.x writes time.Time parameters via
// time.Time.String(), producing several formats across the dataset
// depending on what stripped the monotonic clock + which location
// the time was in. We try the union of every format we've seen in
// production:
//
//   - Go default + monotonic suffix:
//     "2026-05-12 14:55:53.167221395 +0000 UTC m=+1046.173573732"
//   - Go default, monotonic stripped (by .UTC(), .Round(0), JSON, etc.):
//     "2026-05-12 14:55:53.167221395 +0000 UTC"
//   - Go default with fixed-offset location (offset twice):
//     "2026-05-13 20:28:43.247266 +0200 +0200"
//   - RFC3339Nano / RFC3339 (in case any row came from JSON unmarshal):
//     "2026-05-13T18:55:14.529011150Z"
//
// fallbackULID is consulted when the string is empty or unparseable —
// ULIDs encode a millisecond timestamp directly in their first 48
// bits, which is good enough to keep the row chronologically sane.
// Last-ditch fallback is time.Now() so the migration NEVER fails on
// a single bad row.
func parseLegacyTimeMs(s, fallbackULID string) int64 {
	s = strings.TrimSpace(s)
	// Strip "m=+..." / "m=-..." monotonic clock suffix.
	if i := strings.Index(s, " m=+"); i >= 0 {
		s = s[:i]
	} else if i := strings.Index(s, " m=-"); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return fallbackTimeMs(fallbackULID)
	}
	// Idempotency: a pure-digit string is already INTEGER millis
	// (CAST(int AS TEXT) post-v3). Pass through unchanged so a
	// partially-committed v3 migration can re-run without
	// rewriting timestamps via fallbackTimeMs.
	if isAllDigits(s) {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n
		}
	}
	for _, layout := range legacyTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixMilli()
		}
	}
	return fallbackTimeMs(fallbackULID)
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// legacyTimeLayouts is the closed set of formats we'll try on each
// row. Ordered most-specific → least-specific so the right format
// matches before a looser one accidentally swallows it.
var legacyTimeLayouts = []string{
	// Go's default time.String() with named zone (e.g. "UTC", "CEST"):
	"2006-01-02 15:04:05.999999999 -0700 MST",
	"2006-01-02 15:04:05 -0700 MST",
	// Go's default time.String() when location has no IANA name (offset
	// appears twice — the second "+0200" is the FixedZone's name string):
	"2006-01-02 15:04:05.999999999 -0700 -0700",
	"2006-01-02 15:04:05 -0700 -0700",
	// RFC3339(Nano) — likely from JSON-unmarshalled times:
	time.RFC3339Nano,
	time.RFC3339,
}

// fallbackTimeMs extracts the encoded timestamp from a ULID, or
// falls back to time.Now() if the ULID is missing/malformed. ULIDs
// encode milliseconds-since-epoch in their first 48 bits, so this is
// a faithful timestamp for rows whose stored string was corrupted
// but whose ID is intact.
func fallbackTimeMs(id string) int64 {
	if id == "" {
		return time.Now().UnixMilli()
	}
	u, err := ulid.Parse(id)
	if err != nil {
		return time.Now().UnixMilli()
	}
	return int64(u.Time())
}

// timeToMs is the production write-path helper used by Upsert*. Zero
// time.Time is preserved as 0 (so callers that intentionally pass a
// zero value get a zero millis, not "now"). The wrappers in
// sessions.go decide when zero is meaningful vs. needs a default.
func timeToMs(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// msToTime is the production read-path helper. Zero millis maps back
// to a zero time.Time, so a NULL or unset column round-trips faithfully.
func msToTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}
