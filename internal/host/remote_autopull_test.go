package host

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Not parallel: SetWorkRootForTest mutates a package-level override.
func TestMaterializedWorktreeIDs(t *testing.T) {
	root := t.TempDir()
	prev := SetWorkRootForTest(root)
	defer SetWorkRootForTest(prev)

	// Worktree dirs are ULID-named; only those count.
	wtA := "01KWABCDEF0123456789ABCDEF"
	wtB := "01KWABCDEF0123456789ABCDEG"
	for _, id := range []string{wtA, wtB} {
		if err := os.MkdirAll(filepath.Join(root, id), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A stray file is not a worktree and must be ignored.
	if err := os.WriteFile(filepath.Join(root, "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The repos/ subtree holds bare canonicals (repo-first layout), and
	// any other non-ULID dir name isn't a worktree either.
	for _, dir := range []string{"repos", "not-a-ulid"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	ids, err := materializedWorktreeIDs()
	if err != nil {
		t.Fatalf("materializedWorktreeIDs: %v", err)
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []string{wtA, wtB}) {
		t.Errorf("ids = %v, want [%s %s]", ids, wtA, wtB)
	}
}

// A missing work root is "nothing materialized yet", not an error.
func TestMaterializedWorktreeIDs_NoWorkRoot(t *testing.T) {
	prev := SetWorkRootForTest(filepath.Join(t.TempDir(), "does-not-exist"))
	defer SetWorkRootForTest(prev)

	ids, err := materializedWorktreeIDs()
	if err != nil {
		t.Fatalf("materializedWorktreeIDs: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("ids = %v, want empty", ids)
	}
}
