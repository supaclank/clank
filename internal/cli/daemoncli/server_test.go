package daemoncli

import (
	"testing"

	"github.com/acksell/clank/pkg/gateway"
)

// t.Setenv forbids t.Parallel, so these subtests run serially.
func TestLoadTemplatesFromEnv(t *testing.T) {
	t.Run("unset returns nil catalog", func(t *testing.T) {
		t.Setenv("CLANK_TEMPLATES", "")
		got, err := loadTemplatesFromEnv()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Fatalf("want nil, got %#v", got)
		}
	})

	t.Run("parses catalog", func(t *testing.T) {
		t.Setenv("CLANK_TEMPLATES", `[{"id":"expo","display_name":"Expo app","clone_url":"https://example.com/expo.git"}]`)
		got, err := loadTemplatesFromEnv()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []gateway.Template{{ID: "expo", DisplayName: "Expo app", CloneURL: "https://example.com/expo.git"}}
		if len(got) != len(want) || got[0] != want[0] {
			t.Fatalf("want %#v, got %#v", want, got)
		}
	})

	t.Run("invalid JSON fails fast", func(t *testing.T) {
		t.Setenv("CLANK_TEMPLATES", `not json`)
		if _, err := loadTemplatesFromEnv(); err == nil {
			t.Fatal("want error for invalid JSON, got nil")
		}
	})

	t.Run("entry missing clone_url fails fast", func(t *testing.T) {
		t.Setenv("CLANK_TEMPLATES", `[{"id":"expo","display_name":"Expo app"}]`)
		if _, err := loadTemplatesFromEnv(); err == nil {
			t.Fatal("want error for missing clone_url, got nil")
		}
	})

	t.Run("entry missing id fails fast", func(t *testing.T) {
		t.Setenv("CLANK_TEMPLATES", `[{"display_name":"Expo app","clone_url":"https://example.com/expo.git"}]`)
		if _, err := loadTemplatesFromEnv(); err == nil {
			t.Fatal("want error for missing id, got nil")
		}
	})
}
