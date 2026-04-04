package daemon

import (
	"testing"
	"time"
)

func TestParseTimeParam(t *testing.T) {
	t.Parallel()

	t.Run("relative hours", func(t *testing.T) {
		t.Parallel()
		before := time.Now()
		result, err := parseTimeParam("24h")
		after := time.Now()
		if err != nil {
			t.Fatalf("parseTimeParam(24h): %v", err)
		}
		expectedLow := before.Add(-24 * time.Hour)
		expectedHigh := after.Add(-24 * time.Hour)
		if result.Before(expectedLow) || result.After(expectedHigh) {
			t.Errorf("24h: got %v, expected between %v and %v", result, expectedLow, expectedHigh)
		}
	})

	t.Run("relative days", func(t *testing.T) {
		t.Parallel()
		before := time.Now()
		result, err := parseTimeParam("7d")
		after := time.Now()
		if err != nil {
			t.Fatalf("parseTimeParam(7d): %v", err)
		}
		expectedLow := before.Add(-7 * 24 * time.Hour)
		expectedHigh := after.Add(-7 * 24 * time.Hour)
		if result.Before(expectedLow) || result.After(expectedHigh) {
			t.Errorf("7d: got %v, expected between %v and %v", result, expectedLow, expectedHigh)
		}
	})

	t.Run("RFC 3339", func(t *testing.T) {
		t.Parallel()
		result, err := parseTimeParam("2026-03-15T10:30:00Z")
		if err != nil {
			t.Fatalf("parseTimeParam(RFC3339): %v", err)
		}
		expected := time.Date(2026, 3, 15, 10, 30, 0, 0, time.UTC)
		if !result.Equal(expected) {
			t.Errorf("expected %v, got %v", expected, result)
		}
	})

	t.Run("invalid", func(t *testing.T) {
		t.Parallel()
		for _, input := range []string{"", "x", "abc", "7x", "0d", "-3d"} {
			_, err := parseTimeParam(input)
			if err == nil {
				t.Errorf("expected error for %q, got nil", input)
			}
		}
	})
}

func TestContainsAtWordBoundary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		hay  string
		term string
		want bool
	}{
		// Basic match at start of string.
		{"authentication bug", "auth", true},
		// Match at start of a word (after space).
		{"fix authentication", "auth", true},
		// Term in the middle of a word — should NOT match.
		{"they finished", "hey", false},
		// Term at a word boundary — should match.
		{"hey there", "hey", true},
		// Term after punctuation — should match.
		{"bug:auth failed", "auth", true},
		// Term after hyphen — should match.
		{"pre-auth check", "auth", true},
		// Exact match, whole string.
		{"auth", "auth", true},
		// No match at all.
		{"something else", "auth", false},
		// Multiple occurrences: first is mid-word, second is at boundary.
		{"oauth auth-token", "auth", true},
		// Multiple occurrences: all mid-word.
		{"oauth preauth", "auth", false},
		// Term after digit — should match (digit is not a letter).
		{"v2auth", "auth", true},
		// Term after underscore — should match (underscore is not a letter).
		{"pre_auth", "auth", true},
	}

	for _, tt := range tests {
		t.Run(tt.hay+"_"+tt.term, func(t *testing.T) {
			t.Parallel()
			got := containsAtWordBoundary(tt.hay, tt.term)
			if got != tt.want {
				t.Errorf("containsAtWordBoundary(%q, %q) = %v, want %v", tt.hay, tt.term, got, tt.want)
			}
		})
	}
}
