package preview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ensurePreviewShim writes both the shim and the runtime (matching the embedded
// bytes) into one temp dir, and is idempotent across calls.
func TestEnsurePreviewShim(t *testing.T) {
	t.Parallel()

	shimPath, runtimePath, err := ensurePreviewShim()
	if err != nil {
		t.Fatalf("ensurePreviewShim: %v", err)
	}

	if filepath.Dir(shimPath) != filepath.Dir(runtimePath) {
		t.Errorf("shim and runtime should share a dir; got %q and %q", shimPath, runtimePath)
	}
	if filepath.Base(shimPath) != shimFileName {
		t.Errorf("shim basename = %q, want %q", filepath.Base(shimPath), shimFileName)
	}
	if filepath.Base(runtimePath) != runtimeFileName {
		t.Errorf("runtime basename = %q, want %q", filepath.Base(runtimePath), runtimeFileName)
	}

	shim, err := os.ReadFile(shimPath)
	if err != nil {
		t.Fatalf("read shim: %v", err)
	}
	if string(shim) != clankMetroShimJS {
		t.Errorf("written shim does not match embedded clankMetroShimJS")
	}
	// Sanity: the shim is the real thing, not empty.
	if !strings.Contains(string(shim), "InitializeCore") {
		t.Errorf("shim missing the InitializeCore injection marker")
	}

	rt, err := os.ReadFile(runtimePath)
	if err != nil {
		t.Fatalf("read runtime: %v", err)
	}
	if string(rt) != previewRuntimeJS {
		t.Errorf("written runtime does not match embedded previewRuntimeJS")
	}

	// Idempotent: a second call rewrites in place and returns the same paths.
	shimPath2, runtimePath2, err := ensurePreviewShim()
	if err != nil {
		t.Fatalf("ensurePreviewShim (second): %v", err)
	}
	if shimPath2 != shimPath || runtimePath2 != runtimePath {
		t.Errorf("non-idempotent paths: (%q,%q) then (%q,%q)", shimPath, runtimePath, shimPath2, runtimePath2)
	}
}
