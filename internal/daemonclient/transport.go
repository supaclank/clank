package daemonclient

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

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
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
		return fmt.Errorf("daemon returned status %d: %s", resp.StatusCode, summarizeBody(resp.Header.Get("Content-Type"), respBody))
	}

	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// summarizeBody returns a single-line error summary fit for a TUI
// banner: <title> for HTML, otherwise the body collapsed to one line
// and capped at 240 chars. Prevents dumping a 6KB 404 page into the
// inbox's error banner.
func summarizeBody(contentType string, body []byte) string {
	const maxLen = 240
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	switch ct {
	case "text/html", "application/xhtml+xml":
		if title := htmlTitle(body); title != "" {
			return title
		}
	}
	s := strings.Join(strings.Fields(string(body)), " ")
	if len(s) > maxLen {
		s = s[:maxLen] + "…"
	}
	return s
}

// htmlTitle is a tiny <title>X</title> extractor — enough for
// "404 | Sprites" / "Bad Gateway" without an HTML parser.
func htmlTitle(body []byte) string {
	s := string(body)
	lo := strings.Index(strings.ToLower(s), "<title")
	if lo < 0 {
		return ""
	}
	gt := strings.Index(s[lo:], ">")
	if gt < 0 {
		return ""
	}
	start := lo + gt + 1
	end := strings.Index(strings.ToLower(s[start:]), "</title>")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(s[start : start+end])
}

// openSSE GETs path on a no-timeout client so request timeouts can't
// kill long-lived streams. Cloning DefaultTransport keeps Proxy/Idle/
// TLS defaults. Caller closes resp.Body.
func (c *Client) openSSE(ctx context.Context, path string) (*http.Response, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if c.sockPath != "" {
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", c.sockPath)
		}
	}
	sseClient := &http.Client{Transport: transport}
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}
	resp, err := sseClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connect to event stream: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
		resp.Body.Close()
		summary := summarizeBody(resp.Header.Get("Content-Type"), body)
		if summary != "" {
			return nil, fmt.Errorf("event stream returned status %d: %s", resp.StatusCode, summary)
		}
		return nil, fmt.Errorf("event stream returned status %d", resp.StatusCode)
	}
	return resp, nil
}

// parseSSEStream reads SSE events from r into ch. Uses bufio.Reader
// (not Scanner) to avoid the line-length cap that bites long sessions.
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
