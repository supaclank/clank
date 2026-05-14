package store

import (
	"strconv"
	"testing"
	"time"
)

// TestParseLegacyTimeMs_ProductionFormats locks in the parser against
// every input format we've observed in production sprites' host.db.
// If a new format appears in the wild, add it here with a comment
// quoting the source row.
func TestParseLegacyTimeMs_ProductionFormats(t *testing.T) {
	t.Parallel()

	want := time.Date(2026, 5, 12, 14, 55, 53, 167221395, time.UTC).UnixMilli()
	cases := []struct {
		name  string
		input string
		want  int64
	}{
		{
			// modernc.org/sqlite v1.x writes time.Now() this way —
			// the m=+... suffix is the monotonic clock reading.
			name:  "Go String() with monotonic suffix",
			input: "2026-05-12 14:55:53.167221395 +0000 UTC m=+1046.173573732",
			want:  want,
		},
		{
			name:  "Go String() with monotonic minus suffix",
			input: "2026-05-12 14:55:53.167221395 +0000 UTC m=-12.345",
			want:  want,
		},
		{
			// Post-.UTC(), .Round(0), or JSON-round-trip — monotonic
			// is stripped but the named-zone format remains.
			name:  "Go String() UTC named",
			input: "2026-05-12 14:55:53.167221395 +0000 UTC",
			want:  want,
		},
		{
			// FixedZone locations stringify with the offset both as
			// the zone offset and as the zone name.
			name:  "Go String() fixed-offset location",
			input: "2026-05-12 16:55:53.167221395 +0200 +0200",
			want:  want,
		},
		{
			// In case a row was written via a JSON-unmarshalled time.
			name:  "RFC3339Nano",
			input: "2026-05-12T14:55:53.167221395Z",
			want:  want,
		},
		{
			name:  "RFC3339 second precision",
			input: "2026-05-12T14:55:53Z",
			want:  time.Date(2026, 5, 12, 14, 55, 53, 0, time.UTC).UnixMilli(),
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := parseLegacyTimeMs(c.input, "")
			if got != c.want {
				t.Errorf("parseLegacyTimeMs(%q) = %d, want %d (diff %d ms)",
					c.input, got, c.want, got-c.want)
			}
		})
	}
}

// TestParseLegacyTimeMs_NumericPassThrough pins idempotency: a
// pure-digit string is already INTEGER millis (the CAST(int AS TEXT)
// shape produced by re-running the v3 migration on an
// already-migrated table), and must pass through unchanged rather
// than fall back to the ULID/now() path. Regression for
// half-committed v3 migrations that silently rewrote timestamps.
func TestParseLegacyTimeMs_NumericPassThrough(t *testing.T) {
	t.Parallel()
	cases := []int64{
		0,
		1,
		1716000000000,
		time.Now().UnixMilli(),
	}
	for _, want := range cases {
		s := strconv.FormatInt(want, 10)
		got := parseLegacyTimeMs(s, "")
		if got != want {
			t.Errorf("parseLegacyTimeMs(%q) = %d, want %d", s, got, want)
		}
	}
}

// TestParseLegacyTimeMs_FallsBackToULIDTime ensures that an unparseable
// or empty string still produces a sane chronologically-correct
// timestamp, by extracting the encoded millis from the row's ULID.
func TestParseLegacyTimeMs_FallsBackToULIDTime(t *testing.T) {
	t.Parallel()

	// ULID 01KREAZB05YBB9P6QVH0SRZC1F encodes a specific millisecond.
	// We can't hand-compute it, but we can assert the parser produces
	// SOMETHING close-ish to the actual encoded time (within minutes
	// of the source-of-truth user reported), and is deterministic.
	const ulidStr = "01KREAZB05YBB9P6QVH0SRZC1F"
	a := parseLegacyTimeMs("garbage", ulidStr)
	b := parseLegacyTimeMs("", ulidStr)
	c := parseLegacyTimeMs("garbage", ulidStr)
	if a != b || a != c {
		t.Errorf("ULID-fallback not deterministic: a=%d b=%d c=%d", a, b, c)
	}
	// Sanity: encoded time should be in the past, well after epoch.
	if a < time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli() {
		t.Errorf("ULID-derived timestamp %d looks like pre-2024; ulid parse may have failed", a)
	}
}

// TestParseLegacyTimeMs_FallsBackToNowOnInvalidULID covers the
// last-ditch path: no parse, no valid ULID → time.Now(). The exact
// value isn't asserted (it's non-deterministic), only that we get a
// recent positive int64.
func TestParseLegacyTimeMs_FallsBackToNowOnInvalidULID(t *testing.T) {
	t.Parallel()
	before := time.Now().UnixMilli()
	got := parseLegacyTimeMs("garbage", "not-a-ulid")
	after := time.Now().UnixMilli()
	if got < before-100 || got > after+100 {
		t.Errorf("expected ~now millis, got %d (window %d..%d)", got, before, after)
	}
}

// TestTimeRoundTrip_StripsMonotonic empirically pins the boundary
// helper: time.Now() → timeToMs → msToTime survives losslessly at
// millisecond granularity, with the monotonic clock and the original
// location both removed. This is the property we depend on to make
// modernc.org/sqlite happy.
func TestTimeRoundTrip_StripsMonotonic(t *testing.T) {
	t.Parallel()
	now := time.Now()
	ms := timeToMs(now)
	got := msToTime(ms)
	if got.UnixMilli() != ms {
		t.Errorf("round-trip drifted: in=%d out=%d", ms, got.UnixMilli())
	}
	if got.Location() != time.UTC {
		t.Errorf("round-trip location = %v, want UTC", got.Location())
	}
}

// TestTimeRoundTrip_ZeroPreserved ensures a zero time.Time round-trips
// to zero so callers can distinguish "unset" from "epoch".
func TestTimeRoundTrip_ZeroPreserved(t *testing.T) {
	t.Parallel()
	if got := timeToMs(time.Time{}); got != 0 {
		t.Errorf("zero time.Time → %d, want 0", got)
	}
	if got := msToTime(0); !got.IsZero() {
		t.Errorf("0 ms → %v, want zero time.Time", got)
	}
}
