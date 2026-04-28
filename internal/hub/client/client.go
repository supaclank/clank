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
	"log"
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

// Client communicates with clankd's Hub API over either a Unix
// socket (local) or TCP+bearer (remote). Wire protocol is identical;
// only transport and auth differ.
type Client struct {
	sockPath   string // empty when in TCP mode
	baseURL    string // "http://daemon" (Unix) or "http://host:port" (TCP)
	authToken  string // bearer token in TCP mode
	httpClient *http.Client
}

// hubResponseHeaderTimeout caps the wait for response headers. Sized
// for /sessions when LaunchHost provisions a sandbox (cold Daytona
// launches routinely take minutes).
const hubResponseHeaderTimeout = 5 * time.Minute

// NewClient connects to a local clankd via Unix socket. Use NewTCPClient for remote.
// No overall Timeout — long-poll/SSE rely on caller ctx.
func NewClient(sockPath string) *Client {
	return &Client{
		sockPath: sockPath,
		baseURL:  "http://daemon",
		httpClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sockPath)
				},
				ResponseHeaderTimeout: hubResponseHeaderTimeout,
			},
		},
	}
}

// NewTCPClient creates a client that talks to a remote clankd over
// TCP. baseURL must be the externally-reachable hub URL (no trailing
// slash); authToken is sent as `Authorization: Bearer <token>` on
// every request and must match the remote hub's
// preferences.remote_hub.auth_token.
func NewTCPClient(baseURL, authToken string) *Client {
	// Clone DefaultTransport to keep Proxy/Idle/TLS defaults.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = hubResponseHeaderTimeout
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		authToken:  authToken,
		httpClient: &http.Client{Transport: tr},
	}
}

// Process-wide CLI-flag override populated by SetOverride before any
// client is constructed; takes priority over preferences.json.
var (
	overrideURL   string
	overrideToken string
)

// SetOverride sets the CLI-flag-level hub transport override.
// Empty url restores preferences.json as the source of truth.
func SetOverride(url, token string) {
	overrideURL = url
	overrideToken = token
}

// ResetOverride clears any active override.
func ResetOverride() {
	overrideURL = ""
	overrideToken = ""
}

// OverrideURL returns the CLI-flag override URL, or "" if none.
func OverrideURL() string {
	return strings.TrimSpace(overrideURL)
}

// NewDefaultClient creates a client using, in priority order:
//  1. CLI-flag overrides (SetOverride).
//  2. preferences.ActiveHub == "remote" with a configured remote_hub URL.
//  3. The local Unix socket.
//
// Use NewLocalClient for daemon-control commands instead.
func NewDefaultClient() (*Client, error) {
	if strings.TrimSpace(overrideURL) != "" {
		return NewTCPClient(overrideURL, overrideToken), nil
	}
	prefs, err := config.LoadPreferences()
	if err != nil {
		// A corrupt prefs file must surface, not silently fall back to local.
		return nil, fmt.Errorf("load preferences: %w", err)
	}
	if prefs.ActiveHub == "remote" && prefs.RemoteHub != nil && strings.TrimSpace(prefs.RemoteHub.URL) != "" {
		return NewTCPClient(prefs.RemoteHub.URL, prefs.RemoteHub.AuthToken), nil
	}
	sockPath, err := SocketPath()
	if err != nil {
		return nil, err
	}
	return NewClient(sockPath), nil
}

// NewLocalClient returns a Unix-socket client for the local clankd,
// ignoring ActiveHub. Use for daemon-control (start/stop/status).
func NewLocalClient() (*Client, error) {
	sockPath, err := SocketPath()
	if err != nil {
		return nil, err
	}
	return NewClient(sockPath), nil
}

// IsRemoteActive reports whether NewDefaultClient would target a
// remote hub. Logs and degrades to false on prefs load failure.
func IsRemoteActive() bool {
	if strings.TrimSpace(overrideURL) != "" {
		return true
	}
	prefs, err := config.LoadPreferences()
	if err != nil {
		log.Printf("hubclient.IsRemoteActive: prefs load failed (%v) — assuming local; fix the file to switch hubs", err)
		return false
	}
	return prefs.ActiveHub == "remote" && prefs.RemoteHub != nil && strings.TrimSpace(prefs.RemoteHub.URL) != ""
}

// ActiveHubLabel returns a short human-readable description of the
// hub NewDefaultClient would target. Logs and labels as broken on
// prefs load failure.
func ActiveHubLabel() string {
	if u := strings.TrimSpace(overrideURL); u != "" {
		return "override (" + u + ")"
	}
	prefs, err := config.LoadPreferences()
	if err != nil {
		log.Printf("hubclient.ActiveHubLabel: prefs load failed: %v", err)
		return "unknown (prefs unreadable)"
	}
	if prefs.ActiveHub == "remote" && prefs.RemoteHub != nil && strings.TrimSpace(prefs.RemoteHub.URL) != "" {
		return "remote (" + prefs.RemoteHub.URL + ")"
	}
	return "local"
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
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/ping", nil)
	if err != nil {
		return err
	}
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
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
