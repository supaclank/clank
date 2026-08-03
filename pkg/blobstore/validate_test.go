package blobstore_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/supaclank/clank/pkg/blobstore"
)

func TestValidateComponent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"plain", "user-A", false},
		{"ulid", "01JABCDEF0123456789ABCDEFG", false},
		{"empty", "", true},
		{"slash", "a/b", true},
		{"backslash", "a\\b", true},
		{"dotdot", "..", true},
		{"embedded dotdot", "a..b", true},
		{"leading dot", ".hidden", true},
		{"too long", strings.Repeat("a", 129), true},
		{"max len ok", strings.Repeat("a", 128), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := blobstore.ValidateComponent("comp", c.value)
			if c.wantErr {
				if !errors.Is(err, blobstore.ErrInvalidPathComponent) {
					t.Fatalf("want ErrInvalidPathComponent, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidateContentHash(t *testing.T) {
	t.Parallel()
	good := strings.Repeat("a", 64)
	if err := blobstore.ValidateContentHash(good); err != nil {
		t.Fatalf("valid 64-hex rejected: %v", err)
	}
	bad := []string{
		"",
		strings.Repeat("a", 63),
		strings.Repeat("a", 65),
		strings.Repeat("A", 64), // uppercase not allowed
		strings.Repeat("g", 64), // non-hex
	}
	for _, h := range bad {
		if !errors.Is(blobstore.ValidateContentHash(h), blobstore.ErrInvalidPathComponent) {
			t.Fatalf("expected error for %q", h)
		}
	}
}
