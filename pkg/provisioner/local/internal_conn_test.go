package local_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/acksell/clank/pkg/provisioner/hoststore"
	"github.com/acksell/clank/pkg/provisioner/local"
)

func TestGetHostByID_MatchingChild(t *testing.T) {
	t.Parallel()
	bin := fakeHostBin(t)
	p := local.New(local.Options{BinPath: bin, ProvisionTimeout: 5 * time.Second}, nil)
	t.Cleanup(p.Stop)

	ref, err := p.EnsureHost(context.Background(), "")
	if err != nil {
		t.Fatalf("EnsureHost: %v", err)
	}
	if ref.HostID == "" {
		t.Fatal("EnsureHost returned empty HostID")
	}

	got, err := p.GetHostByID(context.Background(), ref.HostID)
	if err != nil {
		t.Fatalf("GetHostByID: %v", err)
	}
	if got.HostID != ref.HostID {
		t.Errorf("HostID = %q, want %q", got.HostID, ref.HostID)
	}
	if got.URL != ref.URL {
		t.Errorf("URL = %q, want %q", got.URL, ref.URL)
	}
	if got.AuthToken != ref.AuthToken {
		t.Errorf("AuthToken differs across EnsureHost/GetHostByID")
	}
}

func TestGetHostByID_NonMatchingHostID(t *testing.T) {
	t.Parallel()
	bin := fakeHostBin(t)
	p := local.New(local.Options{BinPath: bin, ProvisionTimeout: 5 * time.Second}, nil)
	t.Cleanup(p.Stop)

	if _, err := p.EnsureHost(context.Background(), ""); err != nil {
		t.Fatalf("EnsureHost: %v", err)
	}
	_, err := p.GetHostByID(context.Background(), "no-such-host-id")
	if !errors.Is(err, hoststore.ErrHostNotFound) {
		t.Errorf("got %v, want ErrHostNotFound", err)
	}
}

func TestGetHostByID_NoChildRunning(t *testing.T) {
	t.Parallel()
	bin := fakeHostBin(t)
	p := local.New(local.Options{BinPath: bin, ProvisionTimeout: 5 * time.Second}, nil)
	t.Cleanup(p.Stop)
	// EnsureHost intentionally NOT called — no current child.
	_, err := p.GetHostByID(context.Background(), "anything")
	if !errors.Is(err, hoststore.ErrHostNotFound) {
		t.Errorf("got %v, want ErrHostNotFound", err)
	}
}

func TestOpenInternalConn_DialsLocalPort(t *testing.T) {
	t.Parallel()
	bin := fakeHostBin(t)
	p := local.New(local.Options{BinPath: bin, ProvisionTimeout: 5 * time.Second}, nil)
	t.Cleanup(p.Stop)

	// Stand up an httptest server (representing a per-worktree Metro)
	// and verify the local provisioner's OpenInternalConn dials it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello " + r.URL.Path))
	}))
	t.Cleanup(srv.Close)

	port := serverPort(t, srv.URL)
	ref, err := p.EnsureHost(context.Background(), "")
	if err != nil {
		t.Fatalf("EnsureHost: %v", err)
	}

	conn, err := p.OpenInternalConn(context.Background(), ref.HostID, port)
	if err != nil {
		t.Fatalf("OpenInternalConn: %v", err)
	}
	defer conn.Close()

	// Speak HTTP/1.1 through the dialed conn to prove it actually
	// reaches the server. Using net.Conn directly (not http.Client)
	// matches what previewtunnel's RoundTripper does under the hood.
	if _, err := conn.Write([]byte("GET /path HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	body, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(body), "hello /path") {
		t.Errorf("response body doesn't contain handler output: %q", body)
	}
}

func TestOpenInternalConn_RejectsUnknownHostID(t *testing.T) {
	t.Parallel()
	bin := fakeHostBin(t)
	p := local.New(local.Options{BinPath: bin, ProvisionTimeout: 5 * time.Second}, nil)
	t.Cleanup(p.Stop)

	if _, err := p.EnsureHost(context.Background(), ""); err != nil {
		t.Fatalf("EnsureHost: %v", err)
	}
	_, err := p.OpenInternalConn(context.Background(), "wrong-host-id", 1234)
	if !errors.Is(err, hoststore.ErrHostNotFound) {
		t.Errorf("got %v, want ErrHostNotFound", err)
	}
}

func TestOpenInternalConn_RejectsBadPort(t *testing.T) {
	t.Parallel()
	bin := fakeHostBin(t)
	p := local.New(local.Options{BinPath: bin, ProvisionTimeout: 5 * time.Second}, nil)
	t.Cleanup(p.Stop)

	ref, err := p.EnsureHost(context.Background(), "")
	if err != nil {
		t.Fatalf("EnsureHost: %v", err)
	}
	for _, port := range []int{0, -1, 65536} {
		if _, err := p.OpenInternalConn(context.Background(), ref.HostID, port); err == nil {
			t.Errorf("port=%d: expected validation error, got nil", port)
		}
	}
}

// serverPort extracts the port from an http://127.0.0.1:N URL.
func serverPort(t *testing.T, url string) int {
	t.Helper()
	url = strings.TrimPrefix(url, "http://")
	_, portStr, err := net.SplitHostPort(url)
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	p, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return p
}
