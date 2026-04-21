// Package hubclient is the canonical Go client for talking to clankd's Hub
// HTTP API. The TUI, the clank CLI, and any external Go-based clients
// import this package.
//
// The socket path and PID-file helpers (SocketPath, PIDPath, IsRunning) also
// live here and resolve to "hub.sock" / "hub.pid" inside ~/.clank.
//
// API shape (per hub_host_refactor_code_review.md §7.7):
//
//	c.Host(hostname).Repos(ctx)
//	c.Host(hostname).Repo(gitRef).Branches(ctx)
//	c.Host(hostname).Repo(gitRef).Worktree(branch).Resolve(ctx)
//	c.Host(hostname).Repo(gitRef).Worktree(branch).Remove(ctx, force)
//	c.Host(hostname).Repo(gitRef).Worktree(branch).Merge(ctx, msg)
//	c.Backend(backend).Agents(ctx, projectDir)
//	c.Backend(backend).Models(ctx, projectDir)
//	c.Sessions().Create(ctx, req)
//	c.Sessions().List(ctx)
//	c.Sessions().Search(ctx, params)
//	c.Sessions().Subscribe(ctx)
//	c.Sessions().Discover(ctx, projectDir)
//	c.Session(id).Get(ctx)
//	c.Session(id).Messages(ctx)
//	c.Session(id).Send(ctx, opts)
//	c.Session(id).Abort(ctx) ... etc.
//
// Decision: hub-level Backend(backend) is flat, not host-scoped, because the
// hub multiplexes hosts and picks the host internally for these queries.
// Decision: Session(id).Get(ctx) lives on the id-bound handle (not on
// Sessions()) for symmetry with all other id-bound ops.
package hubclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/config"
)

// Client communicates with clankd's Hub API over its Unix domain socket.
type Client struct {
	sockPath   string
	httpClient *http.Client
}

// NewClient creates a client that connects to clankd at the given socket path.
//
// The http.Client has no overall Timeout; long-poll, SSE, and websocket
// upgrade endpoints rely on caller-supplied context for cancellation.
// Per-request deadlines should be set via ctx.
func NewClient(sockPath string) *Client {
	return &Client{
		sockPath: sockPath,
		httpClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sockPath)
				},
				ResponseHeaderTimeout: 30 * time.Second,
			},
		},
	}
}

// NewDefaultClient creates a client using the default socket path.
func NewDefaultClient() (*Client, error) {
	sockPath, err := SocketPath()
	if err != nil {
		return nil, err
	}
	return NewClient(sockPath), nil
}

// SocketPath returns the Unix socket path for clankd's Hub API.
func SocketPath() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "hub.sock"), nil
}

// PIDPath returns the PID file path for clankd.
func PIDPath() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "hub.pid"), nil
}

// socketAlive returns true if a fresh process is currently accepting on
// the hub socket. Used to avoid deleting a live socket when the PID file
// is corrupt or refers to a recycled PID — the new daemon may have a
// different PID than the file claims, but if the socket answers it is
// alive and we must not unlink it.
func socketAlive() bool {
	sockPath, err := SocketPath()
	if err != nil || sockPath == "" {
		return false
	}
	conn, err := net.DialTimeout("unix", sockPath, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// IsRunning checks if clankd is already running by reading the PID file and
// verifying the process exists. Cleans up stale PID and socket files when
// the recorded process is gone.
func IsRunning() (bool, int, error) {
	pidPath, err := PIDPath()
	if err != nil {
		return false, 0, err
	}
	data, err := os.ReadFile(pidPath)
	if os.IsNotExist(err) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		// Corrupt PID file. Probe the socket before unlinking — a live
		// daemon with a corrupted pid file shouldn't have its socket
		// torn out from under it.
		os.Remove(pidPath)
		if !socketAlive() {
			if sockPath, _ := SocketPath(); sockPath != "" {
				os.Remove(sockPath)
			}
		}
		return false, 0, nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false, pid, nil
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		// EPERM means the process exists but we lack permission to
		// signal it (different uid). The daemon is alive — return
		// running=true so the caller doesn't try to start a second
		// one. Only ESRCH (and the nil-error path that already
		// returned above) prove the PID is gone.
		if errors.Is(err, syscall.EPERM) {
			return true, pid, nil
		}
		// Recorded PID is gone (ESRCH or anything else). Probe the
		// socket: a recycled-PID scenario where a fresh daemon
		// rebinds under a new PID would otherwise lose its socket
		// here.
		os.Remove(pidPath)
		if !socketAlive() {
			if sockPath, _ := SocketPath(); sockPath != "" {
				os.Remove(sockPath)
			}
		}
		return false, pid, nil
	}
	return true, pid, nil
}

// Ping checks if clankd is reachable.
func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://daemon/ping", nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("daemon not reachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon returned status %d", resp.StatusCode)
	}
	return nil
}

// PingResponse is the response from /ping.
type PingResponse struct {
	Status  string `json:"status"`
	PID     int    `json:"pid"`
	Uptime  string `json:"uptime"`
	Version string `json:"version"`
}

// PingInfo returns detailed daemon status.
func (c *Client) PingInfo(ctx context.Context) (*PingResponse, error) {
	var resp PingResponse
	if err := c.get(ctx, "/ping", &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// StatusResponse is the response from /status.
type StatusResponse struct {
	PID      int                 `json:"pid"`
	Uptime   string              `json:"uptime"`
	Sessions []agent.SessionInfo `json:"sessions"`
}

// Status returns clankd status including all managed sessions.
func (c *Client) Status(ctx context.Context) (*StatusResponse, error) {
	var resp StatusResponse
	if err := c.get(ctx, "/status", &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
