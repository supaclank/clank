// Package sharetunnel publishes a local preview upstream on a public
// URL via a Cloudflare quick tunnel (the cloudflared binary).
//
// View-only by design: the tunnel fronts the raw dev server, never the
// webpreview overlay proxy — overlay pages carry the daemon bearer
// token, and a share link grants view access to the app, never control
// of its agent (the same policy pkg/gateway applies to public preview
// routes).
package sharetunnel

import (
	"bufio"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// stopGrace is how long Stop lets cloudflared exit on SIGINT before
// the process is killed.
const stopGrace = 5 * time.Second

// outputTailLines bounds the log tail retained for error reporting.
const outputTailLines = 20

// Tunnel is one running cloudflared process fronting one local origin.
type Tunnel struct {
	// PublicURL is the internet-reachable https origin.
	PublicURL string

	stop context.CancelFunc
	done chan struct{}
	// waitErr is the process exit result; readable once done closes.
	waitErr error
}

// Start launches cloudflared (binary path from FindBinary) fronting
// localURL and returns once the public URL is known. ctx bounds only
// that startup wait — the tunnel runs until Stop.
//
// The Host header of tunneled requests is rewritten to localURL's host
// so dev servers with a host allowlist (e.g. Vite) accept them.
func Start(ctx context.Context, binary string, localURL *url.URL) (*Tunnel, error) {
	procCtx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(procCtx, binary,
		"tunnel", "--no-autoupdate",
		"--url", localURL.String(),
		"--http-host-header", localURL.Host,
	)
	// SIGINT first (clean edge disconnect), kill after stopGrace.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = stopGrace

	// One pipe for both streams: cloudflared logs to stderr, but the
	// URL scan shouldn't care which stream a line arrived on.
	pr, pw, err := os.Pipe()
	if err != nil {
		cancel()
		return nil, err
	}
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		cancel()
		pr.Close()
		pw.Close()
		return nil, fmt.Errorf("start %s: %w", BinaryName, err)
	}
	pw.Close() // the child holds the write end now; EOF tracks its exit

	t := &Tunnel{stop: cancel, done: make(chan struct{})}
	urlCh := make(chan string, 1)
	var tail tailBuffer
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		defer pr.Close()
		// Keeps draining after the URL so the child never blocks on a
		// full pipe.
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			line := sc.Text()
			tail.add(line)
			if u, ok := publicTunnelURL(line); ok {
				select {
				case urlCh <- u:
				default:
				}
			}
		}
	}()
	go func() {
		err := cmd.Wait()
		<-scanDone // tail and urlCh are complete before done closes
		t.waitErr = err
		close(t.done)
	}()

	select {
	case u := <-urlCh:
		t.PublicURL = u
		return t, nil
	case <-t.done:
		t.stop()
		return nil, fmt.Errorf("%s exited before publishing a tunnel URL (%v); output:\n%s", BinaryName, t.waitErr, tail.String())
	case <-ctx.Done():
		t.Stop()
		return nil, fmt.Errorf("waiting for the tunnel URL: %w; %s output:\n%s", ctx.Err(), BinaryName, tail.String())
	}
}

// Stop terminates cloudflared and waits for it to exit.
func (t *Tunnel) Stop() {
	t.stop()
	<-t.done
}

// Done closes when the cloudflared process exits — before Stop, that
// means the public URL just died.
func (t *Tunnel) Done() <-chan struct{} { return t.done }

// Err is the process exit result. Valid only once Done is closed.
func (t *Tunnel) Err() error { return t.waitErr }

// tailBuffer retains the last outputTailLines log lines.
type tailBuffer struct {
	mu    sync.Mutex
	lines []string
}

func (b *tailBuffer) add(line string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = append(b.lines, line)
	if len(b.lines) > outputTailLines {
		b.lines = b.lines[len(b.lines)-outputTailLines:]
	}
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Join(b.lines, "\n")
}
