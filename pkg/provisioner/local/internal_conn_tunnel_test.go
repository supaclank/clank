package local_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/acksell/clank/pkg/provisioner/local"
)

// realHostBin compiles the actual cmd/clank-host binary. Unlike
// fakeHostBin this serves the real hostmux — required to exercise the
// /tunnel endpoint end-to-end.
func realHostBin(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "clank-host")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/acksell/clank/cmd/clank-host")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build clank-host: %v\n%s", err, out)
	}
	return bin
}

// TestOpenInternalConn_TunnelMode drives the exact data path the
// machine-style backends (and the docker dev stack with
// CLANK_LOCAL_TUNNEL_INTERNAL_CONN=true) use: provisioner →
// tunnelclient → real clank-host /tunnel → loopback dev-server port.
func TestOpenInternalConn_TunnelMode(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles the real clank-host binary")
	}
	t.Parallel()

	bin := realHostBin(t)
	p := local.New(local.Options{
		BinPath:            bin,
		DataDir:            t.TempDir(),
		ProvisionTimeout:   30 * time.Second,
		TunnelInternalConn: true,
	}, nil)
	t.Cleanup(p.Stop)

	ref, err := p.EnsureHost(context.Background(), "")
	if err != nil {
		t.Fatalf("EnsureHost: %v", err)
	}

	// A loopback echo listener standing in for a Metro dev server.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := p.OpenInternalConn(ctx, ref.HostID, port)
	if err != nil {
		t.Fatalf("OpenInternalConn (tunnel mode): %v", err)
	}
	defer conn.Close()

	payload := []byte("preview bytes through the real host tunnel")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("echo mismatch through tunnel")
	}
}
