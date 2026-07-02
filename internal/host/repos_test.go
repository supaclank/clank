package host

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSlugForImport(t *testing.T) {
	t.Parallel()
	cases := []struct{ owner, repo, want string }{
		{"acme", "api", "acme__api"},
		{"Acksell", "clank-mobile", "Acksell__clank-mobile"},
		{"a b", "c/d", "a-b__c-d"}, // sanitization collapses junk to '-'
	}
	for _, c := range cases {
		got, err := slugForImport(c.owner, c.repo)
		if err != nil {
			t.Fatalf("slugForImport(%q, %q): %v", c.owner, c.repo, err)
		}
		if got != c.want {
			t.Errorf("slugForImport(%q, %q) = %q, want %q", c.owner, c.repo, got, c.want)
		}
	}
	if _, err := slugForImport("!!!", "api"); err == nil {
		t.Error("slugForImport with unusable owner: err = nil, want error")
	}
}

// Not parallel: SetWorkRootForTest mutates a package-level override.
func TestSlugForName_SuffixesOnCollision(t *testing.T) {
	root := t.TempDir()
	prev := SetWorkRootForTest(root)
	defer SetWorkRootForTest(prev)

	got, err := slugForName("My Todo App")
	if err != nil {
		t.Fatalf("slugForName: %v", err)
	}
	if got != "My-Todo-App" {
		t.Errorf("slug = %q, want My-Todo-App", got)
	}

	// Occupy the slug dir → next mint suffixes.
	if err := os.MkdirAll(filepath.Join(root, reposDirName, "My-Todo-App"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err = slugForName("My Todo App")
	if err != nil {
		t.Fatalf("slugForName (collision): %v", err)
	}
	if got != "My-Todo-App-2" {
		t.Errorf("slug = %q, want My-Todo-App-2", got)
	}

	if _, err := slugForName("!!!"); err == nil {
		t.Error("slugForName with unusable name: err = nil, want error")
	}
}

func TestIsULIDLike(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"01KWABCDEF0123456789ABCDEF", true},
		{"repos", false},
		{"01KWABCDEF0123456789ABCDE", false},   // 25 chars
		{"01KWABCDEF0123456789ABCDEFG", false}, // 27 chars
		{"01kwabcdef0123456789abcdef", false},  // lowercase isn't ULID canonical
		{"01KWABCDEF0123456789ABCDEI", false},  // 'I' not in Crockford alphabet
	}
	for _, c := range cases {
		if got := isULIDLike(c.in); got != c.want {
			t.Errorf("isULIDLike(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
