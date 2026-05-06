// Package local implements provisioner.Provisioner by spawning a
// clank-host subprocess on the local machine. Laptop-mode counterpart
// to daytona / flyio: same Provisioner interface, same HostRef shape,
// different "compute" — a child process bound to a random localhost
// port instead of a remote sandbox.
//
// EnsureHost is idempotent within a daemon lifetime: the first call
// spawns clank-host, subsequent calls return the cached HostRef.
// Subprocesses don't survive daemon restarts — cross-restart
// persistence is the cloud provisioners' job. SuspendHost is a no-op;
// DestroyHost kills the child.
package local

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/acksell/clank/provisioner"
	transportpkg "github.com/acksell/clank/provisioner/transport"
)

// Options configures the local-subprocess provisioner.
type Options struct {
	// BinPath is the absolute path to the clank-host binary. Empty
	// → resolved via PATH at first EnsureHost.
	BinPath string

	// DataDir is passed to clank-host as --data-dir for its host.db.
	// Empty → clank-host uses its own default ($HOME/.clank-host).
	DataDir string

	// ProvisionTimeout caps how long EnsureHost waits for the child
	// to print its bound listen address. Default: 10 seconds.
	ProvisionTimeout time.Duration
}

// Provisioner manages a single persistent clank-host subprocess.
type Provisioner struct {
	opts Options
	log  *log.Logger

	mu      sync.Mutex
	current *child
}

type child struct {
	cmd       *exec.Cmd
	hostID    string
	hostname  string
	url       string
	authToken string
	transport http.RoundTripper
	// exited closes when the child process is reaped by the watcher
	// goroutine. EnsureHost selects on this to detect crashed children
	// (cmd.ProcessState only populates after Wait returns, so without
	// the watcher the cache could never invalidate on its own).
	exited chan struct{}
}

// New constructs a Provisioner. log may be nil.
func New(opts Options, lg *log.Logger) *Provisioner {
	if lg == nil {
		lg = log.Default()
	}
	if opts.ProvisionTimeout == 0 {
		opts.ProvisionTimeout = 10 * time.Second
	}
	return &Provisioner{opts: opts, log: lg}
}

// Stop kills the subprocess if running. Safe to defer.
func (p *Provisioner) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.killCurrentLocked()
}

// EnsureHost implements provisioner.Provisioner. Returns the cached
// HostRef when the child is healthy, otherwise spawns a fresh one.
//
// userID is accepted for interface symmetry with daytona/flyio but
// is ignored — the local provisioner serves a single user. Multi-
// tenant routing is PR 4.
func (p *Provisioner) EnsureHost(_ context.Context, _ string) (provisioner.HostRef, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.current != nil {
		select {
		case <-p.current.exited:
			p.log.Printf("local provisioner: cached child has exited; respawning")
			p.killCurrentLocked()
		default:
			return p.refFromChild(p.current), nil
		}
	}

	bin := p.opts.BinPath
	if bin == "" {
		resolved, err := exec.LookPath("clank-host")
		if err != nil {
			return provisioner.HostRef{}, fmt.Errorf("clank-host not in PATH: %w", err)
		}
		bin = resolved
	}

	authToken, err := generateAuthToken()
	if err != nil {
		return provisioner.HostRef{}, fmt.Errorf("generate auth-token: %w", err)
	}

	args := []string{
		"--listen", "tcp://127.0.0.1:0",
		"--listen-auth-token", authToken,
	}
	if p.opts.DataDir != "" {
		if err := os.MkdirAll(p.opts.DataDir, 0o700); err != nil {
			return provisioner.HostRef{}, fmt.Errorf("create data dir %s: %w", p.opts.DataDir, err)
		}
		args = append(args, "--data-dir", p.opts.DataDir)
	}

	cmd := exec.Command(bin, args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return provisioner.HostRef{}, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return provisioner.HostRef{}, fmt.Errorf("start clank-host: %w", err)
	}

	addr, err := waitForListenLine(stderr, p.opts.ProvisionTimeout)
	if err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		return provisioner.HostRef{}, err
	}
	go drainTagged(stderr, p.log, "[clank-host:"+addr+"] ")

	httpURL := strings.Replace(addr, "tcp://", "http://", 1)
	id := ulid.Make().String()
	hostname := "local-" + id[len(id)-8:]
	hostID := ulid.Make().String()

	parsedURL, err := url.Parse(httpURL)
	if err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		return provisioner.HostRef{}, fmt.Errorf("parse local host URL %q: %w", httpURL, err)
	}
	transport := &transportpkg.BearerInjector{Token: authToken, Host: parsedURL.Host}
	c := &child{
		cmd:       cmd,
		hostID:    hostID,
		hostname:  hostname,
		url:       httpURL,
		authToken: authToken,
		transport: transport,
		exited:    make(chan struct{}),
	}
	// Reap the child in the background so c.exited closes when the
	// process dies (whether from a graceful Stop or an unexpected
	// crash). Without this, EnsureHost could never invalidate the cache
	// after a crash and would return stale URLs forever.
	go func() {
		_ = cmd.Wait()
		close(c.exited)
	}()
	p.current = c
	p.log.Printf("local provisioner: spawned host %s at %s (data-dir=%q)", hostname, httpURL, p.opts.DataDir)
	return p.refFromChild(c), nil
}

func (p *Provisioner) refFromChild(c *child) provisioner.HostRef {
	return provisioner.HostRef{
		HostID:    c.hostID,
		URL:       c.url,
		Transport: c.transport,
		AuthToken: c.authToken,
		AutoWake:  false,
		Hostname:  c.hostname,
	}
}

// SuspendHost is a no-op: subprocesses don't auto-suspend.
func (p *Provisioner) SuspendHost(context.Context, string) error {
	return nil
}

// DestroyHost kills the subprocess and clears the cache.
func (p *Provisioner) DestroyHost(_ context.Context, _ string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.killCurrentLocked()
	return nil
}

// killCurrentLocked sends SIGINT, waits 5s, then SIGKILLs. Watches
// c.exited (the watcher goroutine spawned by EnsureHost) instead of
// calling cmd.Wait directly — concurrent Wait calls are undefined.
// Caller holds p.mu.
func (p *Provisioner) killCurrentLocked() {
	c := p.current
	if c == nil {
		return
	}
	p.current = nil
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Signal(os.Interrupt)
	}
	select {
	case <-c.exited:
	case <-time.After(5 * time.Second):
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		<-c.exited
	}
}

func generateAuthToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func waitForListenLine(r io.Reader, timeout time.Duration) (string, error) {
	type result struct {
		addr string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		s := bufio.NewScanner(r)
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

func drainTagged(r io.Reader, lg *log.Logger, prefix string) {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for s.Scan() {
		lg.Printf("%s%s", prefix, s.Text())
	}
}

var _ provisioner.Provisioner = (*Provisioner)(nil)
