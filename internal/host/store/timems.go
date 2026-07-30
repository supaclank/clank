package store

import "time"

// Time columns are stored as INTEGER unix milliseconds (see schema.sql
// for why DATETIME-as-TEXT is a trap with modernc.org/sqlite). These two
// helpers are the only conversion points between time.Time and the
// stored representation.

// timeToMs is the write-path helper used by Upsert*. Zero time.Time is
// preserved as 0 (so callers that intentionally pass a zero value get a
// zero millis, not "now"). The wrappers in sessions.go decide when zero
// is meaningful vs. needs a default.
func timeToMs(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// msToTime is the read-path helper. Zero millis maps back to a zero
// time.Time, so a NULL or unset column round-trips faithfully.
func msToTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}
