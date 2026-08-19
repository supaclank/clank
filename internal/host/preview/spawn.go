package preview

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// ringCapacity is the size of the per-server stdout/stderr ring. 64 KiB
// captures a comfortable amount of Metro's startup chatter (~30 lines
// of bundle progress + a stack trace if anything blew up) without
// turning a runaway dev server into a memory leak.
const ringCapacity = 64 * 1024

// startupLogCommandFormat is the announcement line format for a
// configured preview's startup command (see running.startupLine).
const startupLogCommandFormat = "$ %s\n"

// readyTimeout caps how long Manager.Start waits for the dev server to
// pass its readiness probe. On a freshly-materialized worktree the spawn
// command runs `bun install` FIRST (node_modules is gitignored, so it's
// fetched on the first preview) before Metro starts — a cold install of a
// large app can take several minutes — so this budget is generous and wraps
// install + start. A genuinely crashed dev server is caught immediately via
// the process-exit path (r.done closes); this limit only applies while the
// child process is still alive. Overridable per spawn via spawnRequest.ReadyTimeout.
const readyTimeout = 10 * time.Minute

// gracefulCancelDelay is how long a context-canceled child (readiness
// timeout, or the caller bailing out) gets to exit after SIGTERM before
// Go's WaitDelay escalates to SIGKILL. A SIGKILL mid-install leaves a
// half-extracted node_modules (forcing the bootstrap marker's full
// clean-reinstall next run), so give it a clean shot to unwind first.
const gracefulCancelDelay = 10 * time.Second

// spawnRequest carries everything spawn needs that isn't on the Spec.
// The struct is internal — Manager.Start unpacks its method args here.
type spawnRequest struct {
	WorkDir     string
	Spec        Spec
	ServiceName string

	// Port is the OS-allocated port the child should listen on. Manager
	// allocates upfront (before the gateway register webhook) so the
	// public URL can be threaded into PublicURL below.
	Port int

	// PublicURL is the externally-reachable URL the gateway will route
	// to this dev server, e.g. "http://preview-<token>.<root>:7878".
	// Expo receives it as the packager proxy URL. Web previews derive their
	// public hostname and readiness Host header from it. Empty selects local mode.
	PublicURL string

	// ReadyTimeout overrides readyTimeout for this spawn. Zero means
	// "use the package default." Test seam — production callers leave
	// this zero.
	ReadyTimeout time.Duration

	// ShimRequirePath / RuntimePath wire the guest-side preview runtime (Layer A)
	// into `expo start` via NODE_OPTIONS=--require <shim>, with the runtime path
	// passed as CLANK_PREVIEW_RUNTIME. Both empty disables injection (non-Expo,
	// or when ensurePreviewShim failed). See inject.go + clank-metro-shim.js.
	ShimRequirePath string
	RuntimePath     string
}

// spawn launches the dev server described by req and returns a
// populated *running record. The record's state is Starting on return;
// a goroutine spawned inside watches stdout for the readiness pattern
// and flips state to Ready (or Failed on early exit / timeout).
//
// The returned *running owns the wait goroutine; Manager.Stop is the
// only legal way to reap it. Failing-during-startup paths inside spawn
// itself are responsible for SIGKILL'ing the partial child before
// returning the error.
func spawn(ctx context.Context, req spawnRequest) (*running, error) {
	if req.Port == 0 {
		return nil, fmt.Errorf("spawn: port is required")
	}
	port := req.Port

	args, err := renderSpecArgs(req.Spec, port)
	if err != nil {
		return nil, err
	}

	var markerPath string
	if req.Spec.Kind == KindExpo {
		markerPath, err = bootstrapMarkerPath(req.WorkDir)
		if err != nil {
			return nil, err
		}
	}
	env, readinessHost, err := prepareEnvironment(environmentRequest{
		Kind:            req.Spec.Kind,
		MarkerPath:      markerPath,
		PublicURL:       req.PublicURL,
		ShimRequirePath: req.ShimRequirePath,
		RuntimePath:     req.RuntimePath,
		Port:            port,
		Configured:      req.Spec.Environment,
	})
	if err != nil {
		return nil, err
	}

	// Tie the child to a per-spawn context so Stop's cancel() can kick
	// in if the process is still in startup when the user bails out.
	childCtx, cancel := context.WithCancel(ctx)

	cmd := exec.CommandContext(childCtx, args[0], args[1:]...)
	cmd.Dir = req.WorkDir
	cmd.Env = env
	configureProcessGroup(cmd)

	logs := newRingBuf(ringCapacity)
	cmd.Stdout = logs
	cmd.Stderr = logs

	// Kept outside the ring (see running.startupLine) so a chatty child
	// can't evict the announcement once combined output exceeds ringCapacity.
	var startupLine []byte
	if req.Spec.StartupLogCommand != "" {
		line := fmt.Sprintf(startupLogCommandFormat, strings.TrimSpace(req.Spec.StartupLogCommand))
		startupLine = sanitizeTerminalOutput([]byte(line))
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start %s: %w", args[0], err)
	}

	r := &running{
		spec:        req.Spec,
		serviceName: req.ServiceName,
		port:        port,
		state:       StateStarting,
		startedAt:   time.Now(),
		lastTouch:   time.Now(),
		logs:        logs,
		startupLine: startupLine,
		pid:         cmd.Process.Pid,
		pgid:        cmd.Process.Pid, // Setpgid: true → pgid == pid
		done:        make(chan struct{}),
		cancel:      cancel,
	}

	// Reap in the background so r.done closes when the child dies for
	// any reason (graceful Stop, crash, ctx cancel). Without this, Stop
	// would have to call cmd.Wait itself and would race with the
	// readiness loop's own observation of stdin EOF.
	go func() {
		_ = cmd.Wait()
		r.mu.Lock()
		// A child that exits before flipping to Ready is treated as a
		// startup failure; one that exits after is just a normal stop.
		if r.state == StateStarting {
			r.state = StateFailed
			if r.lastErr == "" {
				r.lastErr = "process exited during startup"
			}
		} else if r.state == StateReady {
			r.state = StateStopped
		}
		r.pid, r.pgid = 0, 0
		r.mu.Unlock()
		close(r.done)
	}()

	// Poll for readiness. A separate goroutine so spawn returns
	// immediately with State=Starting and the caller can either poll
	// /status or subscribe to the SSE log stream (future).
	timeout := req.ReadyTimeout
	if timeout == 0 {
		timeout = readyTimeout
	}
	go probeReady(r, req.Spec.ReadyProbe, readinessHost, timeout)

	return r, nil
}

func renderSpecArgs(spec Spec, port int) ([]string, error) {
	if !spec.ShouldSubstitutePort {
		return append([]string(nil), spec.CmdTemplate...), nil
	}
	return renderArgs(spec.CmdTemplate, port)
}

// allocatePort opens a TCP listener on an OS-chosen port, closes it,
// and returns the freed number. There's a tiny TOCTOU window between
// close and the child binding — on a single-tenant sprite this is
// fine. If we ever see EADDRINUSE in practice we'll add a retry loop.
func allocatePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		return 0, err
	}
	return port, nil
}

// renderArgs substitutes "%d" in the spec template with port. A
// template that mentions %d zero times is fine (the framework picks
// the port itself, e.g. via env), but more than one match is rejected
// as a config bug.
func renderArgs(tmpl []string, port int) ([]string, error) {
	out := make([]string, 0, len(tmpl))
	matches := 0
	for _, arg := range tmpl {
		if strings.Contains(arg, "%d") {
			matches++
			arg = strings.ReplaceAll(arg, "%d", fmt.Sprintf("%d", port))
		}
		out = append(out, arg)
	}
	if matches > 1 {
		return nil, fmt.Errorf("spec.CmdTemplate contains %d %%d placeholders, want at most 1", matches)
	}
	return out, nil
}

// probeReady polls http://127.0.0.1:<r.port><probe.Path> every 200ms
// using requestHost until it returns 200 AND the body contains probe.ExpectedSubstr
// (when set). Flips r.state to Ready on success, to Failed on
// timeout. Exits early if r.done closes (child died before we saw
// readiness — leave state to the wait goroutine).
//
// Mirrors OpenCodeServerManager.waitForReady's shape — concrete HTTP
// check beats stdout scanning, which raced against Python's
// print-before-bind and shifted between Expo SDK versions.
func probeReady(r *running, probe ReadyProbe, requestHost string, timeout time.Duration) {
	if probe.Path == "" {
		// No probe configured — go straight to Ready. Defensive; today
		// every Spec carries a probe.
		r.mu.Lock()
		if r.state == StateStarting {
			r.state = StateReady
		}
		r.mu.Unlock()
		return
	}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", r.port, probe.Path)
	expect := []byte(probe.ExpectedSubstr)
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if probeOnce(client, url, expect, requestHost) {
			r.mu.Lock()
			if r.state == StateStarting {
				r.state = StateReady
			}
			r.mu.Unlock()
			return
		}
		select {
		case <-r.done:
			return
		case <-ticker.C:
			if time.Now().After(deadline) {
				r.mu.Lock()
				if r.state == StateStarting {
					r.state = StateFailed
					r.lastErr = fmt.Sprintf("readiness probe %s did not return expected response within %s", url, timeout)
				}
				r.mu.Unlock()
				// Tear down the still-starting child via the spawn
				// context so the wait goroutine reaps it. Don't call
				// stopProcessGroup here — Manager.Stop is the canonical
				// shutdown path and we don't want two races.
				r.cancel()
				return
			}
		}
	}
}

// probeOnce returns true when the URL responds 200 for requestHost and the body
// contains expect. An empty requestHost or expect leaves that check unchanged.
//
// Body read is capped at 1 KiB — enough for Metro's "packager-status:running"
// without buffering an unbounded response from a misbehaving server.
func probeOnce(client *http.Client, url string, expect []byte, requestHost string) bool {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	if requestHost != "" {
		req.Host = requestHost
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	if len(expect) == 0 {
		return true
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return false
	}
	return bytes.Contains(body, expect)
}
