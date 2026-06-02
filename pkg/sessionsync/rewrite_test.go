package sessionsync

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRewriteImportBlob(t *testing.T) {
	t.Parallel()
	src := filepath.Join(t.TempDir(), "blob.json")
	orig := map[string]any{
		"info": map[string]any{
			"id":        "ses_abc",
			"title":     "hello world",
			"directory": "/src/host/path",
			"projectID": "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
			"version":   "1.3.15",
		},
		"messages": []any{map[string]any{"role": "user"}},
	}
	raw, err := json.Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	const destDir = "/dest/work/wt_123"
	out, err := RewriteImportBlob(src, destDir)
	if err != nil {
		t.Fatalf("RewriteImportBlob: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(out) })

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	info := doc["info"].(map[string]any)

	if info["directory"] != destDir {
		t.Errorf("directory = %v, want %q", info["directory"], destDir)
	}
	if info["projectID"] != "" {
		t.Errorf("projectID = %v, want empty (force rederive)", info["projectID"])
	}
	// Untouched fields preserved verbatim.
	if info["id"] != "ses_abc" {
		t.Errorf("id = %v, want ses_abc (preserved)", info["id"])
	}
	if info["title"] != "hello world" {
		t.Errorf("title = %v, want preserved", info["title"])
	}
	if info["version"] != "1.3.15" {
		t.Errorf("version = %v, want preserved", info["version"])
	}
	if doc["messages"] == nil {
		t.Error("messages dropped, want preserved")
	}
}

func TestRewriteImportBlob_MissingInfo(t *testing.T) {
	t.Parallel()
	src := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(src, []byte(`{"messages":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := RewriteImportBlob(src, "/dest"); err == nil {
		t.Fatal("expected error for blob missing top-level info object")
	}
}
