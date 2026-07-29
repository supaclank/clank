package clankcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreviewFileArg(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	note := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(note, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("existing file selects file mode", func(t *testing.T) {
		t.Parallel()
		file, ok, err := previewFileArg([]string{note})
		if err != nil || !ok || file != note {
			t.Fatalf("got (%q, %v, %v), want (%q, true, nil)", file, ok, err, note)
		}
	})

	t.Run("multi-word args stay a prompt", func(t *testing.T) {
		t.Parallel()
		if _, ok, err := previewFileArg([]string{"fix", "the", "readme.md"}); ok || err != nil {
			t.Fatalf("got (ok=%v, err=%v), want prompt flow", ok, err)
		}
	})

	t.Run("single non-path word stays a prompt", func(t *testing.T) {
		t.Parallel()
		if _, ok, err := previewFileArg([]string{"refactor"}); ok || err != nil {
			t.Fatalf("got (ok=%v, err=%v), want prompt flow", ok, err)
		}
	})

	t.Run("path-shaped but missing is a hard error", func(t *testing.T) {
		// A typo'd path must never silently become an agent prompt.
		t.Parallel()
		if _, ok, err := previewFileArg([]string{filepath.Join(dir, "missing.md")}); ok || err == nil {
			t.Fatalf("got (ok=%v, err=%v), want error", ok, err)
		}
	})

	t.Run("directory is a hard error", func(t *testing.T) {
		t.Parallel()
		if _, ok, err := previewFileArg([]string{dir}); ok || err == nil {
			t.Fatalf("got (ok=%v, err=%v), want error", ok, err)
		}
	})

	t.Run("no args stay a prompt", func(t *testing.T) {
		t.Parallel()
		if _, ok, err := previewFileArg(nil); ok || err != nil {
			t.Fatalf("got (ok=%v, err=%v), want prompt flow", ok, err)
		}
	})
}

func TestProjectRelPath(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(project, "docs", "a.md")
	if err := os.WriteFile(inside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "b.md")
	if err := os.WriteFile(outside, []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("inside resolves slash-relative", func(t *testing.T) {
		t.Parallel()
		rel, err := projectRelPath(project, inside)
		if err != nil || rel != "docs/a.md" {
			t.Fatalf("got (%q, %v), want (docs/a.md, nil)", rel, err)
		}
	})

	t.Run("outside is rejected", func(t *testing.T) {
		t.Parallel()
		if _, err := projectRelPath(project, outside); err == nil || !strings.Contains(err.Error(), "outside the project") {
			t.Fatalf("err = %v, want outside-the-project rejection", err)
		}
	})

	t.Run("symlink-aliased project dir compares equal", func(t *testing.T) {
		// macOS spells temp dirs /var/… but realpaths them to
		// /private/var/…; both spellings must resolve the same file.
		t.Parallel()
		alias := filepath.Join(t.TempDir(), "alias")
		if err := os.Symlink(project, alias); err != nil {
			t.Fatal(err)
		}
		rel, err := projectRelPath(alias, filepath.Join(alias, "docs", "a.md"))
		if err != nil || rel != "docs/a.md" {
			t.Fatalf("got (%q, %v), want (docs/a.md, nil)", rel, err)
		}
	})

	t.Run("in-project symlink pointing out is rejected", func(t *testing.T) {
		t.Parallel()
		link := filepath.Join(project, "leak.md")
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		if _, err := projectRelPath(project, link); err == nil {
			t.Fatal("want rejection for a symlink escaping the project")
		}
	})
}
