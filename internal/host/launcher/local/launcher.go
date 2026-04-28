// Package local implements a HostLauncher that spawns clank-host
// subprocesses on a local TCP port. It is the dev/test stub for the
// "fresh sandbox per session" flow — useful for exercising the whole
// pipeline (sync → launch → clone-from-mirror → backend) on one
// machine before the Daytona launcher (Step 7) is available.
//
// The launcher tracks every spawned child so its Stop() can clean them
// all up at hub shutdown.
package local

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
	hostclient "github.com/acksell/clank/internal/host/client"
	"github.com/oklog/ulid/v2"
)

// Launcher spawns clank-host subprocesses on demand.
type Launcher struct {
	opts Options
	log  *log.Logger

	mu       sync.Mutex
	children []*child
}

// Options configures the Launcher.
type Options struct {
	// BinPath is the path to the clank-host binary. Empty resolves via
	// PATH at Launch time.
	BinPath string

	// GitSyncSource is the cloud-hub base URL the spawned clank-host
	// should clone from. Empty (the default) means the spawned host
	// clones directly from each session's RemoteURL — useful for
	// laptop-side dev runs but not for sandbox emulation.
	GitSyncSource string

	// GitSyncToken is the bearer token paired with GitSyncSource.
	GitSyncToken string
}

type child struct {
	cmd  *exec.Cmd
	addr string // tcp://host:port the child bound to
	name host.Hostname
}

// New constructs a Launcher. Pass an Options zero-value for the
// simplest "spawn clank-host, no sync source" configuration.
func New(opts Options, lg *log.Logger) *Launcher {
	if lg == nil {
		lg = log.Default()
	}
	return &Launcher{opts: opts, log: lg}
}

// Launch spawns a clank-host listening on a random TCP port, waits
// for it to print its bound address on stderr, and returns a host
// client pointed at it.
func (l *Launcher) Launch(ctx context.Context, _ agent.LaunchHostSpec) (host.Hostname, *hostclient.HTTP, error) {
	bin := l.opts.BinPath
	if bin == "" {
		resolved, err := exec.LookPath("clank-host")
		if err != nil {
			return "", nil, fmt.Errorf("clank-host not in PATH: %w", err)
		}
		bin = resolved
	}

	args := []string{"--listen", "tcp://127.0.0.1:0"}
	if l.opts.GitSyncSource != "" {
		args = append(args, "--git-sync-source", l.opts.GitSyncSource)
	}
	if l.opts.GitSyncToken != "" {
		args = append(args, "--git-sync-token", l.opts.GitSyncToken)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return "", nil, fmt.Errorf("start clank-host: %w", err)
	}

	addr, err := waitForListenLine(stderr, 5*time.Second)
	if err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		return "", nil, err
	}
	// Continue draining stderr so the child doesn't block on a full
	// pipe. Tag the lines so multi-launcher logs stay legible.
	go drainTagged(stderr, l.log, "[clank-host:"+addr+"] ")

	httpURL := strings.Replace(addr, "tcp://", "http://", 1)
	// Use the random tail of the ULID — the first 10 chars are the
	// timestamp prefix and collide for any two launches in the same
	// millisecond, which `RegisterHost` would treat as the same host.
	id := ulid.Make().String()
	name := host.Hostname("local-" + id[len(id)-8:])
	client := hostclient.NewHTTP(httpURL, nil)

	l.mu.Lock()
	l.children = append(l.children, &child{cmd: cmd, addr: addr, name: name})
	l.mu.Unlock()

	l.log.Printf("local launcher: spawned host %s at %s", name, httpURL)
	return name, client, nil
}

// Stop signals every spawned clank-host to shut down and waits up to
// 5 seconds per child for it to exit. Safe to call from a defer.
func (l *Launcher) Stop() {
	l.mu.Lock()
	children := l.children
	l.children = nil
	l.mu.Unlock()
	for _, c := range children {
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Signal(os.Interrupt)
		}
	}
	for _, c := range children {
		done := make(chan struct{})
		go func() {
			_ = c.cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			if c.cmd.Process != nil {
				_ = c.cmd.Process.Kill()
			}
			<-done
		}
	}
}

// waitForListenLine reads from the child's stderr until it sees the
// "listening on tcp://addr:port" line printed by clank-host. Returns
// the URL ("tcp://addr:port") or an error if no line appears within
// the timeout.
//
// We deliberately read line-by-line so unrelated log output before the
// listen line doesn't confuse us — clank-host's log prefix lets us
// match precisely.
func waitForListenLine(r io.Reader, timeout time.Duration) (string, error) {
	type result struct {
		addr string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		s := bufio.NewScanner(r)
		// Bigger buffer than default in case logs are wide.
		s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for s.Scan() {
			line := s.Text()
			if i := strings.Index(line, "listening on tcp://"); i >= 0 {
				addr := strings.TrimSpace(line[i+len("listening on "):])
				ch <- result{addr: addr}
				return
			}
		}
		if err := s.Err(); err != nil {
			ch <- result{err: err}
		} else {
			ch <- result{err: io.EOF}
		}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return "", fmt.Errorf("clank-host did not announce listen addr: %w", r.err)
		}
		return r.addr, nil
	case <-time.After(timeout):
		return "", fmt.Errorf("timed out waiting for clank-host listen line")
	}
}

// drainTagged forwards r's lines to lg with a prefix. Stops on EOF.
func drainTagged(r io.Reader, lg *log.Logger, prefix string) {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for s.Scan() {
		lg.Printf("%s%s", prefix, s.Text())
	}
}
