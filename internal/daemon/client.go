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
	"github.com/coder/websocket"
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

// SearchSessions searches session metadata (title, prompt, draft, project
// name) using case-insensitive word-boundary matching. The query supports
// pipe-separated OR groups with space-separated AND terms within each group.
// Since/Until filter on UpdatedAt using relative durations (e.g. "7d") or
// RFC 3339 timestamps.
func (c *Client) SearchSessions(ctx context.Context, p agent.SearchParams) ([]agent.SessionInfo, error) {
	v := url.Values{}
	if p.Query != "" {
		v.Set("q", p.Query)
	}
	if !p.Since.IsZero() {
		v.Set("since", p.Since.Format(time.RFC3339))
	}
	if !p.Until.IsZero() {
		v.Set("until", p.Until.Format(time.RFC3339))
	}
	if p.Visibility != "" {
		v.Set("visibility", string(p.Visibility))
	}
	var sessions []agent.SessionInfo
	if err := c.get(ctx, "/sessions/search?"+v.Encode(), &sessions); err != nil {
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

// ServerStatus is a running OpenCode server with its session count.
type ServerStatus struct {
	URL          string    `json:"url"`
	ProjectDir   string    `json:"project_dir"`
	PID          int       `json:"pid"`
	StartedAt    time.Time `json:"started_at"`
	SessionCount int       `json:"session_count"`
}

// ListOpenCodeServers returns running OpenCode server processes.
func (c *Client) ListOpenCodeServers(ctx context.Context) ([]ServerStatus, error) {
	var servers []ServerStatus
	if err := c.get(ctx, "/debug/opencode/servers", &servers); err != nil {
		return nil, err
	}
	return servers, nil
}

// AbortSession interrupts a running session.
func (c *Client) AbortSession(ctx context.Context, sessionID string) error {
	return c.post(ctx, "/sessions/"+sessionID+"/abort", nil, nil)
}

// RevertSession reverts a session to the specified message, removing all
// subsequent messages.
func (c *Client) RevertSession(ctx context.Context, sessionID, messageID string) error {
	body := struct {
		MessageID string `json:"message_id"`
	}{MessageID: messageID}
	return c.post(ctx, "/sessions/"+sessionID+"/revert", body, nil)
}

// ForkSession forks a session from the given message, creating a new session
// with the message history up to (but not including) the specified message.
// If messageID is empty, the entire session is forked.
func (c *Client) ForkSession(ctx context.Context, sessionID, messageID string) (*agent.SessionInfo, error) {
	body := struct {
		MessageID string `json:"message_id,omitempty"`
	}{MessageID: messageID}
	var info agent.SessionInfo
	if err := c.post(ctx, "/sessions/"+sessionID+"/fork", body, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// MarkSessionRead marks a session as read by setting its LastReadAt timestamp.
func (c *Client) MarkSessionRead(ctx context.Context, sessionID string) error {
	return c.post(ctx, "/sessions/"+sessionID+"/read", nil, nil)
}

// ToggleFollowUp toggles the follow-up flag on a session and returns the new state.
func (c *Client) ToggleFollowUp(ctx context.Context, sessionID string) (bool, error) {
	var resp struct {
		FollowUp bool `json:"follow_up"`
	}
	if err := c.post(ctx, "/sessions/"+sessionID+"/followup", nil, &resp); err != nil {
		return false, err
	}
	return resp.FollowUp, nil
}

// SetVisibility sets the visibility state of a session (visible, done, or archived).
func (c *Client) SetVisibility(ctx context.Context, sessionID string, visibility agent.SessionVisibility) error {
	body := struct {
		Visibility agent.SessionVisibility `json:"visibility"`
	}{Visibility: visibility}
	return c.post(ctx, "/sessions/"+sessionID+"/visibility", body, nil)
}

// SetDraft sets or clears the draft text for a session.
// Pass an empty string to clear the draft.
func (c *Client) SetDraft(ctx context.Context, sessionID string, draft string) error {
	body := struct {
		Draft string `json:"draft"`
	}{Draft: draft}
	return c.post(ctx, "/sessions/"+sessionID+"/draft", body, nil)
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
// Uses bufio.Reader instead of bufio.Scanner to avoid a hard upper limit on
// line length — Scanner fails permanently with "token too long" on large
// payloads (common with session snapshots for long conversations).
func parseSSEStream(r io.Reader, ch chan<- agent.Event) {
	reader := bufio.NewReader(r)
	var eventType string
	var dataLines []string

	for {
		line, err := reader.ReadBytes('\n')
		lineStr := strings.TrimRight(string(line), "\r\n")

		if lineStr == "" && err == nil {
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

		if lineStr != "" {
			if strings.HasPrefix(lineStr, "event: ") {
				eventType = strings.TrimPrefix(lineStr, "event: ")
			} else if strings.HasPrefix(lineStr, "data: ") {
				dataLines = append(dataLines, strings.TrimPrefix(lineStr, "data: "))
			}
		}

		if err != nil {
			// Dispatch any buffered event before exiting.
			if eventType != "" && len(dataLines) > 0 {
				data := strings.Join(dataLines, "\n")
				if eventType != "connected" {
					var evt agent.Event
					if err := json.Unmarshal([]byte(data), &evt); err == nil {
						ch <- evt
					}
				}
			}
			if err != io.EOF {
				// Report the error so the TUI can display it instead
				// of silently losing the event stream.
				ch <- agent.Event{
					Type:      agent.EventError,
					Timestamp: time.Now(),
					Data:      agent.ErrorData{Message: "SSE stream: " + err.Error()},
				}
			}
			return
		}
	}
}

// ReplyPermission replies to a permission request.
func (c *Client) ReplyPermission(ctx context.Context, sessionID, permissionID string, allow bool) error {
	body := map[string]bool{"allow": allow}
	return c.post(ctx, "/sessions/"+sessionID+"/permissions/"+permissionID+"/reply", body, nil)
}

// --- Voice methods ---

// VoiceStop tears down the active voice session.
func (c *Client) VoiceStop(ctx context.Context) error {
	return c.post(ctx, "/voice/stop", nil, nil)
}

// VoiceStatusResponse is the response from GET /voice/status.
type VoiceStatusResponse struct {
	Active string `json:"active"`
	Status string `json:"status"`
}

// VoiceStatus returns the current voice session state.
func (c *Client) VoiceStatus(ctx context.Context) (*VoiceStatusResponse, error) {
	var resp VoiceStatusResponse
	if err := c.get(ctx, "/voice/status", &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// VoiceAudioStream opens a WebSocket connection for bidirectional PCM
// audio streaming. The caller sends mic PCM as binary messages and
// receives speaker PCM back. A zero-length binary message from the
// server signals a flush (barge-in / discard playback buffer).
//
// The returned *websocket.Conn must be closed by the caller.
func (c *Client) VoiceAudioStream(ctx context.Context) (*websocket.Conn, error) {
	conn, _, err := websocket.Dial(ctx, "ws://daemon/voice/audio", &websocket.DialOptions{
		HTTPClient: c.httpClient,
	})
	if err != nil {
		return nil, fmt.Errorf("voice audio websocket: %w", err)
	}
	conn.SetReadLimit(256 * 1024)
	return conn, nil
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
