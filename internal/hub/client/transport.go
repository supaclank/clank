package hubclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/acksell/clank/internal/agent"
)

// HTTP request helpers used by all sub-clients. The wire format is
// JSON-in/JSON-out with a small set of error-code conventions.

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

// openSSE opens an SSE GET to path on a no-timeout client and returns
// the response. Caller is responsible for closing resp.Body.
//
// We allocate a fresh *http.Client (and *http.Transport) per call rather
// than reusing c.httpClient because the latter has request timeouts
// that would prematurely close long-lived event streams. The cost is
// one transport per subscription; in practice the TUI subscribes once
// per process, so this is not a hot path. If callers ever start
// subscribing repeatedly, hoist a single SSE-tuned client onto Client
// (idle conns disabled, no Timeout) instead of caching this one.
func (c *Client) openSSE(ctx context.Context, path string) (*http.Response, error) {
	sseClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", c.sockPath)
			},
		},
	}
	req, err := http.NewRequestWithContext(ctx, "GET", "http://daemon"+path, nil)
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
	return resp, nil
}

// parseSSEStream reads SSE events from r and sends them to ch. Uses
// bufio.Reader instead of bufio.Scanner to avoid the hard line-length cap
// (Scanner permanently fails on session snapshots for long conversations).
func parseSSEStream(r io.Reader, ch chan<- agent.Event) {
	reader := bufio.NewReader(r)
	var eventType string
	var dataLines []string

	for {
		line, err := reader.ReadBytes('\n')
		lineStr := strings.TrimRight(string(line), "\r\n")

		if lineStr == "" && err == nil {
			if eventType != "" && len(dataLines) > 0 {
				data := strings.Join(dataLines, "\n")
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
