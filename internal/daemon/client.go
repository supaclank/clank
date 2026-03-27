package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/acksell/clank/internal/agent"
)

// Client communicates with the daemon over its Unix domain socket.
type Client struct {
	sockPath   string
	httpClient *http.Client
}

// NewClient creates a client that connects to the daemon at the given socket path.
func NewClient(sockPath string) *Client {
	return &Client{
		sockPath: sockPath,
		httpClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sockPath)
				},
			},
			Timeout: 30 * time.Second,
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

// Ping checks if the daemon is reachable.
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

// PingResponse is the response from the ping endpoint.
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

// StatusResponse is the response from the status endpoint.
type StatusResponse struct {
	PID      int                 `json:"pid"`
	Uptime   string              `json:"uptime"`
	Sessions []agent.SessionInfo `json:"sessions"`
}

// Status returns the daemon status including all managed sessions.
func (c *Client) Status(ctx context.Context) (*StatusResponse, error) {
	var resp StatusResponse
	if err := c.get(ctx, "/status", &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// CreateSession asks the daemon to create and start a new agent session.
func (c *Client) CreateSession(ctx context.Context, req agent.StartRequest) (*agent.SessionInfo, error) {
	var info agent.SessionInfo
	if err := c.post(ctx, "/sessions", req, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// ListSessions returns all managed sessions.
func (c *Client) ListSessions(ctx context.Context) ([]agent.SessionInfo, error) {
	var sessions []agent.SessionInfo
	if err := c.get(ctx, "/sessions", &sessions); err != nil {
		return nil, err
	}
	return sessions, nil
}

// GetSession returns a single session by ID.
func (c *Client) GetSession(ctx context.Context, id string) (*agent.SessionInfo, error) {
	var info agent.SessionInfo
	if err := c.get(ctx, "/sessions/"+id, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// GetSessionMessages returns the full message history for a session.
func (c *Client) GetSessionMessages(ctx context.Context, sessionID string) ([]agent.MessageData, error) {
	var messages []agent.MessageData
	if err := c.get(ctx, "/sessions/"+sessionID+"/messages", &messages); err != nil {
		return nil, err
	}
	return messages, nil
}

// SendMessage sends a follow-up message to a running session.
func (c *Client) SendMessage(ctx context.Context, sessionID string, opts agent.SendMessageOpts) error {
	return c.post(ctx, "/sessions/"+sessionID+"/message", opts, nil)
}

// ListAgents returns available agents for the given backend and project directory.
func (c *Client) ListAgents(ctx context.Context, backend agent.BackendType, projectDir string) ([]agent.AgentInfo, error) {
	path := "/agents?backend=" + url.QueryEscape(string(backend)) + "&project_dir=" + url.QueryEscape(projectDir)
	var agents []agent.AgentInfo
	if err := c.get(ctx, path, &agents); err != nil {
		return nil, err
	}
	return agents, nil
}

// AbortSession interrupts a running session.
func (c *Client) AbortSession(ctx context.Context, sessionID string) error {
	return c.post(ctx, "/sessions/"+sessionID+"/abort", nil, nil)
}

// MarkSessionRead marks a session as read by setting its LastReadAt timestamp.
func (c *Client) MarkSessionRead(ctx context.Context, sessionID string) error {
	return c.post(ctx, "/sessions/"+sessionID+"/read", nil, nil)
}

// DeleteSession stops and removes a session.
func (c *Client) DeleteSession(ctx context.Context, sessionID string) error {
	return c.do(ctx, "DELETE", "/sessions/"+sessionID, nil, nil)
}

// DiscoverSessions asks the daemon to discover and register historical sessions
// from the OpenCode backend for the given project directory.
func (c *Client) DiscoverSessions(ctx context.Context, projectDir string) error {
	body := struct {
		ProjectDir string `json:"project_dir"`
	}{ProjectDir: projectDir}
	return c.post(ctx, "/sessions/discover", body, nil)
}

// SubscribeEvents opens an SSE stream and delivers events to the returned channel.
// The channel is closed when the context is cancelled or the connection drops.
func (c *Client) SubscribeEvents(ctx context.Context) (<-chan agent.Event, error) {
	// Use a separate client without timeout for long-lived SSE.
	sseClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", c.sockPath)
			},
		},
		// No timeout — SSE connections are long-lived.
	}

	req, err := http.NewRequestWithContext(ctx, "GET", "http://daemon/events", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := sseClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connect to event stream: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("event stream returned status %d", resp.StatusCode)
	}

	ch := make(chan agent.Event, 64)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		parseSSEStream(resp.Body, ch)
	}()

	return ch, nil
}

// parseSSEStream reads SSE events from the reader and sends them to the channel.
func parseSSEStream(r io.Reader, ch chan<- agent.Event) {
	scanner := bufio.NewScanner(r)
	var eventType string
	var dataLines []string

	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			// Empty line = end of event.
			if eventType != "" && len(dataLines) > 0 {
				data := strings.Join(dataLines, "\n")
				// Skip the "connected" event.
				if eventType != "connected" {
					var evt agent.Event
					if err := json.Unmarshal([]byte(data), &evt); err == nil {
						ch <- evt
					}
				}
			}
			eventType = ""
			dataLines = nil
			continue
		}

		if strings.HasPrefix(line, "event: ") {
			eventType = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
		}
	}
}

// ReplyPermission replies to a permission request.
func (c *Client) ReplyPermission(ctx context.Context, requestID, reply string) error {
	body := map[string]string{"reply": reply}
	return c.post(ctx, "/permissions/"+requestID+"/reply", body, nil)
}

// --- HTTP helpers ---

func (c *Client) get(ctx context.Context, path string, out interface{}) error {
	return c.do(ctx, "GET", path, nil, out)
}

func (c *Client) post(ctx context.Context, path string, body interface{}, out interface{}) error {
	return c.do(ctx, "POST", path, body, out)
}

func (c *Client) do(ctx context.Context, method, path string, body interface{}, out interface{}) error {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, "http://daemon"+path, bodyReader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("daemon request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		var errResp map[string]string
		if json.Unmarshal(respBody, &errResp) == nil {
			if msg, ok := errResp["error"]; ok {
				return fmt.Errorf("daemon: %s", msg)
			}
		}
		return fmt.Errorf("daemon returned status %d: %s", resp.StatusCode, respBody)
	}

	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}
