package webpreview

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestOverlayFrameFocus(t *testing.T) {
	t.Parallel()
	chrome, err := exec.LookPath("google-chrome")
	if err != nil {
		t.Skip("google-chrome not installed; skipping overlay browser test")
	}
	fixture, err := os.ReadFile("testdata/overlay-focus.html")
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/test-result" {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			select {
			case results <- string(body):
			case <-r.Context().Done():
			}
			return
		}
		if ServeOverlayAsset(w, r) {
			return
		}
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(fixture)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, chrome,
		"--headless", "--no-sandbox", "--disable-dev-shm-usage",
		"--no-first-run", "--no-default-browser-check", "--disable-background-networking",
		"--user-data-dir="+t.TempDir(), server.URL,
	)
	cmd.WaitDelay = time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case result := <-results:
		cancel()
		<-exited
		if result != "PASS" {
			t.Fatalf("overlay focus checks failed:\n%s", result)
		}
	case err := <-exited:
		t.Fatalf("Chrome exited without a test result: %v\n%s", err, stderr.String())
	}
}
