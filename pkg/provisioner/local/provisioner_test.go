package local_test

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/acksell/clank/provisioner/local"
)

// fakeHostBin compiles a tiny Go program that prints the
// "listening on tcp://127.0.0.1:<port>" line the provisioner expects,
// listens on a real port, and exits after $FAKE_HOST_LIFETIME (default:
// forever). Returns the path to the compiled binary.
//
// The provisioner only cares about (a) the listen-line on stderr and
// (b) the process being alive — there's no clank-host protocol behind
// it for these tests.
func fakeHostBin(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fakeHostBin: TODO Windows support")
	}
	src := `package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

func main() {
	listen := flag.String("listen", "tcp://127.0.0.1:0", "")
	_ = flag.String("listen-auth-token", "", "")
	_ = flag.String("data-dir", "", "")
	flag.Parse()
	addr := strings.TrimPrefix(*listen, "tcp://")
	ln, err := net.Listen("tcp", addr)
	if err != nil { fmt.Fprintln(os.Stderr, "listen:", err); os.Exit(1) }
	fmt.Fprintf(os.Stderr, "listening on tcp://%s\n", ln.Addr().String())
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil { return }
			c.Close()
		}
	}()
	if v := os.Getenv("FAKE_HOST_LIFETIME"); v != "" {
		d, _ := time.ParseDuration(v)
		time.Sleep(d)
		ln.Close()
		return
	}
	select {}
}
`
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(srcPath, []byte(src), 0o644); err != nil {
		t.Fatalf("write fake source: %v", err)
	}
	binPath := filepath.Join(dir, "fake-clank-host")
	cmd := exec.Command("go", "build", "-o", binPath, srcPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake host: %v\n%s", err, out)
	}
	return binPath
}

// TestEnsureHost_DetectsCrashedChild pins the regression CR caught:
// pre-fix EnsureHost relied on cmd.ProcessState which is nil until
// Wait() returns. Without a watcher goroutine, a crashed child would
// keep the cache populated and the provisioner would hand out a stale
// URL forever. The fix adds an exited chan closed by a Wait watcher.
func TestEnsureHost_DetectsCrashedChild(t *testing.T) {
	// No t.Parallel: t.Setenv is incompatible with parallel tests.
	bin := fakeHostBin(t)

	// FAKE_HOST_LIFETIME makes the child exit shortly after spawn so
	// we can deterministically observe the crash-detect path.
	t.Setenv("FAKE_HOST_LIFETIME", "200ms")

	p := local.New(local.Options{BinPath: bin, ProvisionTimeout: 5 * time.Second}, nil)
	t.Cleanup(p.Stop)

	ref1, err := p.EnsureHost(context.Background(), "")
	if err != nil {
		t.Fatalf("first EnsureHost: %v", err)
	}
	if ref1.URL == "" {
		t.Fatal("first EnsureHost returned empty URL")
	}

	// Wait for the fake child to exit (it sleeps 200ms then closes the
	// listener). Poll the URL until accept() fails.
	if !waitListenerGone(ref1.URL, 2*time.Second) {
		t.Fatal("fake host did not stop accepting within 2s; lifetime env not honored?")
	}

	ref2, err := p.EnsureHost(context.Background(), "")
	if err != nil {
		t.Fatalf("second EnsureHost: %v", err)
	}
	if ref2.URL == ref1.URL {
		t.Fatalf("EnsureHost returned the SAME URL %q after the child crashed; the cache wasn't invalidated", ref2.URL)
	}
}

// waitListenerGone returns true once a TCP dial to rawURL stops
// succeeding within timeout.
func waitListenerGone(rawURL string, timeout time.Duration) bool {
	addr := strings.TrimPrefix(rawURL, "http://")
	addr = strings.TrimPrefix(addr, "https://")
	addr = strings.TrimPrefix(addr, "tcp://")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return true
		}
		conn.Close()
		time.Sleep(50 * time.Millisecond)
	}
	return false
}
