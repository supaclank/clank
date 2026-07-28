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

	"github.com/acksell/clank/pkg/provisioner"
	"github.com/acksell/clank/pkg/provisioner/local"
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
	workRoot := flag.String("work-root", "", "")
	templatesJSON := flag.String("templates-json", "", "")
	_ = flag.Bool("local-file-attachments", false, "")
	_ = flag.Bool("gh-cli-auth", false, "")
	_ = flag.Bool("claude-cli-auth", false, "")
	_ = flag.Bool("codex-cli-auth", false, "")
	flag.Parse()
	if f := os.Getenv("FAKE_HOST_WORK_ROOT_FILE"); f != "" {
		if err := os.WriteFile(f, []byte(*workRoot), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "dump work-root:", err)
			os.Exit(1)
		}
	}
	if f := os.Getenv("FAKE_HOST_TEMPLATES_FILE"); f != "" {
		set := false
		flag.Visit(func(fl *flag.Flag) { if fl.Name == "templates-json" { set = true } })
		if err := os.WriteFile(f, []byte(fmt.Sprintf("set=%v value=%s", set, *templatesJSON)), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "dump templates-json:", err)
			os.Exit(1)
		}
	}
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

// TestEnsureHost_PassesWorkRoot: Options.WorkRoot must reach the child
// as --work-root (and the directory must exist before spawn) — the
// laptop daemon relies on this to keep worktrees under the clank
// config dir instead of the sprite-style $HOME/work.
func TestEnsureHost_PassesWorkRoot(t *testing.T) {
	// No t.Parallel: t.Setenv is incompatible with parallel tests.
	bin := fakeHostBin(t)
	workRoot := filepath.Join(t.TempDir(), "clank-work")
	dumpFile := filepath.Join(t.TempDir(), "work-root.txt")
	t.Setenv("FAKE_HOST_WORK_ROOT_FILE", dumpFile)

	p := local.New(local.Options{
		BinPath:          bin,
		WorkRoot:         workRoot,
		ProvisionTimeout: 5 * time.Second,
	}, nil)
	t.Cleanup(p.Stop)

	if _, err := p.EnsureHost(context.Background(), ""); err != nil {
		t.Fatalf("EnsureHost: %v", err)
	}
	if fi, err := os.Stat(workRoot); err != nil || !fi.IsDir() {
		t.Errorf("work root %q not created before spawn: stat err=%v", workRoot, err)
	}
	got, err := os.ReadFile(dumpFile)
	if err != nil {
		t.Fatalf("read child's --work-root dump: %v", err)
	}
	if string(got) != workRoot {
		t.Errorf("child received --work-root %q, want %q", got, workRoot)
	}
}

// TestEnsureHost_PassesTemplatesJSON: Options.Templates must reach the
// child as --templates-json — the laptop daemon relies on this to serve
// its builtin create-project catalog (e.g. the default Expo starter)
// from the spawned clank-host's GET /templates.
func TestEnsureHost_PassesTemplatesJSON(t *testing.T) {
	// No t.Parallel: t.Setenv is incompatible with parallel tests.
	bin := fakeHostBin(t)
	dumpFile := filepath.Join(t.TempDir(), "templates.txt")
	t.Setenv("FAKE_HOST_TEMPLATES_FILE", dumpFile)

	templates := []provisioner.Template{{
		DisplayName: "Expo app",
		CloneURL:    "https://templates.example/expo.git",
	}}
	p := local.New(local.Options{
		BinPath:          bin,
		Templates:        templates,
		ProvisionTimeout: 5 * time.Second,
	}, nil)
	t.Cleanup(p.Stop)

	if _, err := p.EnsureHost(context.Background(), ""); err != nil {
		t.Fatalf("EnsureHost: %v", err)
	}
	got, err := os.ReadFile(dumpFile)
	if err != nil {
		t.Fatalf("read child's --templates-json dump: %v", err)
	}
	want := "set=true value=" + provisioner.TemplatesEnvValue(templates)
	if string(got) != want {
		t.Errorf("child received --templates-json %q, want %q", got, want)
	}
}

// TestEnsureHost_NoTemplatesOmitsFlag: with an empty catalog the flag
// must stay absent so the child's own default (its env / no builtins)
// applies rather than an explicit empty override.
func TestEnsureHost_NoTemplatesOmitsFlag(t *testing.T) {
	// No t.Parallel: t.Setenv is incompatible with parallel tests.
	bin := fakeHostBin(t)
	dumpFile := filepath.Join(t.TempDir(), "templates.txt")
	t.Setenv("FAKE_HOST_TEMPLATES_FILE", dumpFile)

	p := local.New(local.Options{BinPath: bin, ProvisionTimeout: 5 * time.Second}, nil)
	t.Cleanup(p.Stop)

	if _, err := p.EnsureHost(context.Background(), ""); err != nil {
		t.Fatalf("EnsureHost: %v", err)
	}
	got, err := os.ReadFile(dumpFile)
	if err != nil {
		t.Fatalf("read child's --templates-json dump: %v", err)
	}
	// set=false distinguishes a truly absent flag from an explicit
	// --templates-json "" (which would override the child's env default).
	if want := "set=false value="; string(got) != want {
		t.Errorf("child received --templates-json dump %q, want %q", got, want)
	}
}

// TestDestroyHostsByUser_NoChildIsNoOp: account erasure on the local
// provisioner with no running subprocess is a clean no-op (idempotent).
func TestDestroyHostsByUser_NoChildIsNoOp(t *testing.T) {
	t.Parallel()
	p := local.New(local.Options{}, nil)
	if err := p.DestroyHostsByUser(context.Background(), "anyone"); err != nil {
		t.Fatalf("DestroyHostsByUser with no child: %v", err)
	}
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
