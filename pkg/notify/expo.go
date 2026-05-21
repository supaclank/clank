// Package notify implements the clankd-side push delivery for
// host-emitted notifier webhooks. It owns:
//   - the Expo Push API client (this file)
//   - the per-user devices registry (devices.go)
//   - the HTTP dispatcher that ties them together (dispatcher.go)
//
// External services (Expo, APNs, FCM) live behind one Provider-style
// abstraction so self-hosters can swap in their own delivery without
// patching dispatcher logic.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"
)

const (
	// defaultExpoEndpoint is Expo's hosted Push API. Tests override via
	// NewWithEndpoint.
	defaultExpoEndpoint = "https://exp.host/--/api/v2/push/send"

	// maxBatchSize is the upper bound Expo accepts per request. The
	// client splits larger lists into multiple POSTs.
	// https://docs.expo.dev/push-notifications/sending-notifications/
	maxBatchSize = 100

	// httpTimeout caps a single Push round-trip. Long enough for the
	// 99th percentile, short enough that a hung Expo doesn't pile up
	// goroutines in the dispatcher.
	httpTimeout = 10 * time.Second
)

// Priority is the Expo-defined urgency tier. We only emit "high" for
// notifications that should bypass low-power mode (idle / permission /
// error) — everything else stays default.
type Priority string

const (
	PriorityDefault Priority = "default"
	PriorityHigh    Priority = "high"
)

// Message is a single push payload. Mirrors Expo's request shape but
// with Go-friendly types. Callers fill To, Title, Body; the dispatcher
// fills Data (so the mobile client can deep-link) and Priority.
type Message struct {
	To       string         `json:"to"`
	Title    string         `json:"title,omitempty"`
	Body     string         `json:"body,omitempty"`
	Data     map[string]any `json:"data,omitempty"`
	Priority Priority       `json:"priority,omitempty"`
	Sound    string         `json:"sound,omitempty"`
}

// Ticket is Expo's per-message acknowledgement. Status is "ok" or
// "error"; on error, Details.Error categorizes (the canonical value
// we act on is "DeviceNotRegistered" — the token is dead and should
// be purged).
type Ticket struct {
	Status  string `json:"status"`
	ID      string `json:"id,omitempty"`
	Message string `json:"message,omitempty"`
	Details struct {
		Error string `json:"error,omitempty"`
	} `json:"details,omitempty"`
}

// IsDeviceNotRegistered reports whether the ticket indicates a dead
// push token. Callers use this to purge stale devices rows.
func (t Ticket) IsDeviceNotRegistered() bool {
	return t.Status == "error" && t.Details.Error == "DeviceNotRegistered"
}

// Client is the Expo Push API client. Construct with New.
type Client struct {
	endpoint    string
	accessToken string
	http        *http.Client
	log         *log.Logger
}

// New constructs a Client with Expo's hosted endpoint. A nil logger
// uses a stderr-prefixed default.
func New(lg *log.Logger) *Client {
	return NewWithEndpoint(defaultExpoEndpoint, lg)
}

// NewWithEndpoint constructs a Client targeting an arbitrary endpoint.
// Used by tests (httptest.Server) and by self-hosters proxying Expo.
func NewWithEndpoint(endpoint string, lg *log.Logger) *Client {
	if lg == nil {
		lg = log.New(os.Stderr, "[notify-expo] ", log.LstdFlags|log.Lmsgprefix)
	}
	return &Client{
		endpoint: endpoint,
		http:     &http.Client{Timeout: httpTimeout},
		log:      lg,
	}
}

// WithAccessToken sets the Expo Access Token sent as
// "Authorization: Bearer <token>" on every Push call. Production
// deployments mint one at expo.dev → access tokens and supply it via
// env (e.g. EXPO_ACCESS_TOKEN); without it the Push API still accepts
// requests but any caller who steals a push token can abuse it because
// the token is the only routing key. Empty disables the header — fine
// for dev.
//
// Chainable so the wiring stays a one-liner:
//   notify.New(lg).WithAccessToken(os.Getenv("EXPO_ACCESS_TOKEN"))
func (c *Client) WithAccessToken(token string) *Client {
	c.accessToken = token
	return c
}

// Push delivers msgs to Expo, chunking at maxBatchSize. Returns one
// Ticket per input message, in the same order. A whole-batch error
// (HTTP failure) is returned without tickets; per-message errors
// surface inside their tickets so the caller can still purge dead
// tokens for the messages that did get processed.
func (c *Client) Push(ctx context.Context, msgs []Message) ([]Ticket, error) {
	if len(msgs) == 0 {
		return nil, nil
	}
	out := make([]Ticket, 0, len(msgs))
	for start := 0; start < len(msgs); start += maxBatchSize {
		end := start + maxBatchSize
		if end > len(msgs) {
			end = len(msgs)
		}
		tickets, err := c.pushBatch(ctx, msgs[start:end])
		if err != nil {
			return nil, fmt.Errorf("push batch [%d:%d]: %w", start, end, err)
		}
		if len(tickets) != end-start {
			return nil, fmt.Errorf("push batch [%d:%d]: expected %d tickets, got %d", start, end, end-start, len(tickets))
		}
		out = append(out, tickets...)
	}
	return out, nil
}

type expoResponse struct {
	Data   []Ticket   `json:"data"`
	Errors []expoErr  `json:"errors,omitempty"`
}

type expoErr struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (c *Client) pushBatch(ctx context.Context, batch []Message) ([]Ticket, error) {
	body, err := json.Marshal(batch)
	if err != nil {
		return nil, fmt.Errorf("marshal batch: %w", err)
	}
	if _, urlErr := url.Parse(c.endpoint); urlErr != nil {
		return nil, fmt.Errorf("invalid endpoint %q: %w", c.endpoint, urlErr)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.accessToken)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post expo: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("expo POST: status %d body=%q", resp.StatusCode, snippet)
	}
	var er expoResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if len(er.Errors) > 0 {
		// Top-level errors are whole-batch failures (e.g. invalid JSON
		// shape, throttling). Surface the first one — Expo guarantees
		// at most one in practice.
		return nil, fmt.Errorf("expo error: %s — %s", er.Errors[0].Code, er.Errors[0].Message)
	}
	return er.Data, nil
}
