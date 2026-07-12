package webpreview

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestEnsureModelFilesDownloadsAndSkips(t *testing.T) {
	t.Parallel()
	var fetches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		w.Write([]byte("weights for " + r.URL.Path))
	}))
	defer srv.Close()

	dir := t.TempDir()
	files := []modelFile{
		{Name: "a.onnx", URL: srv.URL + "/a"},
		{Name: "tokens.txt", URL: srv.URL + "/t"},
	}

	var reports []string
	progress := func(file string, i, n int, done, total int64) {
		reports = append(reports, file)
	}

	if err := ensureModelFiles(context.Background(), dir, files, progress); err != nil {
		t.Fatalf("ensureModelFiles: %v", err)
	}
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(dir, f.Name))
		if err != nil || len(data) == 0 {
			t.Fatalf("%s: not materialized (%v)", f.Name, err)
		}
	}
	if fetches.Load() != 2 {
		t.Fatalf("fetches = %d, want 2", fetches.Load())
	}
	if len(reports) == 0 {
		t.Fatalf("progress callback never fired")
	}

	// Second run: everything present → no network at all.
	if err := ensureModelFiles(context.Background(), dir, files, nil); err != nil {
		t.Fatalf("second ensureModelFiles: %v", err)
	}
	if fetches.Load() != 2 {
		t.Fatalf("fetches after idempotent run = %d, want still 2", fetches.Load())
	}
}

func TestEnsureModelFilesFailureLeavesNoPartial(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	dir := t.TempDir()
	err := ensureModelFiles(context.Background(), dir, []modelFile{{Name: "bad.onnx", URL: srv.URL + "/bad"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("err = %v, want HTTP 500", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		t.Errorf("failed download left %q behind", e.Name())
	}
}

func TestModelsPresent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if ModelsPresent(dir) {
		t.Fatalf("empty dir must not count as present")
	}
	for _, f := range parakeetFiles {
		if err := os.WriteFile(filepath.Join(dir, f.Name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if !ModelsPresent(dir) {
		t.Fatalf("all files present must count as present")
	}
	// A zero-byte file (crashed old-style download) must not count.
	if err := os.WriteFile(filepath.Join(dir, parakeetFiles[0].Name), nil, 0o644); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if ModelsPresent(dir) {
		t.Fatalf("zero-byte model file must not count as present")
	}
}
