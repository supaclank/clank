package main

import (
	"strings"
	"testing"
)

// Ported from daemoncli's TestLoadTemplatesFromEnv when the builtin
// catalog moved from the gateway to the host: a typo must surface at
// startup, not as a silently-empty picker.
func TestParseTemplatesJSON(t *testing.T) {
	t.Parallel()

	t.Run("empty returns nil catalog", func(t *testing.T) {
		t.Parallel()
		got, err := parseTemplatesJSON("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Fatalf("want nil, got %#v", got)
		}
	})

	t.Run("parses catalog, id optional", func(t *testing.T) {
		t.Parallel()
		got, err := parseTemplatesJSON(`[{"display_name":"Expo app","clone_url":"https://example.com/expo.git"},{"id":"legacy","display_name":"Legacy","clone_url":"https://example.com/legacy.git"}]`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 2 || got[0].DisplayName != "Expo app" || got[1].CloneURL != "https://example.com/legacy.git" {
			t.Fatalf("got %#v", got)
		}
	})

	t.Run("invalid JSON fails fast", func(t *testing.T) {
		t.Parallel()
		if _, err := parseTemplatesJSON(`not json`); err == nil {
			t.Fatal("want error for invalid JSON, got nil")
		}
	})

	t.Run("entry missing clone_url fails fast", func(t *testing.T) {
		t.Parallel()
		_, err := parseTemplatesJSON(`[{"display_name":"Expo app"}]`)
		if err == nil || !strings.Contains(err.Error(), "clone_url") {
			t.Fatalf("want clone_url error, got %v", err)
		}
	})

	t.Run("entry missing display_name fails fast", func(t *testing.T) {
		t.Parallel()
		if _, err := parseTemplatesJSON(`[{"clone_url":"https://example.com/x.git"}]`); err == nil {
			t.Fatal("want display_name error, got nil")
		}
	})
}
