package clankcli

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestPreviewFrontDoorAuthAndProxy boots a real front door in front of a
// stub unix-socket "daemon" and asserts: the pairing token gates the LAN
// boundary (401 without it), authed requests proxy through to the daemon,
// and /v1/images is served locally (LAN blobstore) rather than proxied.
// No mocks — real listeners, real HTTP.
func TestPreviewFrontDoorAuthAndProxy(t *testing.T) {
	t.Parallel()

	// Stub daemon on a unix socket: /ping -> "pong", everything else 204.
	// Bind under /tmp — t.TempDir() paths blow past macOS's ~104-char
	// unix-socket-path limit (bind: invalid argument).
	tmp, err := os.MkdirTemp("/tmp", "cpfd")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	sock := filepath.Join(tmp, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	stub := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ping" {
			_, _ = w.Write([]byte("pong"))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = stub.Serve(ln) }()
	defer func() { _ = stub.Close() }()

	fd, err := startPreviewFrontDoor(net.IPv4(127, 0, 0, 1), 0, sock, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("startPreviewFrontDoor: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		fd.Shutdown(ctx)
	}()

	// No bearer -> 401 at the LAN boundary, never reaches the daemon.
	resp, err := http.Get(fd.BaseURL + "/ping")
	if err != nil {
		t.Fatalf("GET /ping: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-bearer /ping = %d, want 401", resp.StatusCode)
	}

	// Pairing token -> proxied to the daemon -> 200 "pong".
	req, _ := http.NewRequest(http.MethodGet, fd.BaseURL+"/ping", nil)
	req.Header.Set("Authorization", "Bearer "+fd.Token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("authed GET /ping: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "pong" {
		t.Fatalf("authed /ping = %d %q, want 200 \"pong\"", resp.StatusCode, body)
	}

	// /v1/images is served locally (200 presign), not proxied (would be 204).
	img, _ := http.NewRequest(http.MethodPost, fd.BaseURL+"/v1/images", strings.NewReader(`{"mime":"image/png"}`))
	img.Header.Set("Authorization", "Bearer "+fd.Token)
	img.Header.Set("Content-Type", "application/json")
	imgResp, err := http.DefaultClient.Do(img)
	if err != nil {
		t.Fatalf("POST /v1/images: %v", err)
	}
	_ = imgResp.Body.Close()
	if imgResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/images = %d, want 200 (served locally, not proxied)", imgResp.StatusCode)
	}
}
