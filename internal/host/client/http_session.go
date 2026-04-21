package hostclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/acksell/clank/internal/agent"
)

// httpSessionBackend implements agent.SessionBackend by translating each
// method call into an HTTP request against the host. Constructed by
// HTTP.CreateSession; one instance per session.
//
// Status(), SessionID() are served from a local cache guarded by mu.
// The cache is updated by:
//   - Start(), which refreshes externalID+status from the Start response
//     (some backends, e.g. opencode, only learn their real sessionID
//     once Start has opened the remote session).
//   - the Events() goroutine, whenever it observes a status-change event.
type httpSessionBackend struct {
	c         *HTTP
	sessionID string

	mu         sync.RWMutex
	externalID string
	status     agent.SessionStatus

	// Events() is lazy-initialised; eventsCh and eventsOnce guard
	// the single-subscription invariant.
	eventsOnce sync.Once
	eventsCh   chan agent.Event
}

func newHTTPSessionBackend(c *HTTP, sessionID, externalID string, st agent.SessionStatus) *httpSessionBackend {
	return &httpSessionBackend{
		c:          c,
		sessionID:  sessionID,
		externalID: externalID,
		status:     st,
	}
}

// errEmptyHTTPSessionID guards against constructing requests for an
// empty backend session id, which would silently produce malformed
// routes like "/sessions//start".
var errEmptyHTTPSessionID = errors.New("http session backend: empty session id")

// path builds a URL path with the session id properly escaped so ids
// containing reserved characters (slash, question mark, hash, …) or
// empty ids cannot produce malformed routes.
func (b *httpSessionBackend) path(suffix string) (string, error) {
	if b.sessionID == "" {
		return "", errEmptyHTTPSessionID
	}
	return "/sessions/" + url.PathEscape(b.sessionID) + suffix, nil
}

func (b *httpSessionBackend) Start(ctx context.Context, req agent.StartRequest) error {
	// Host mux returns the post-Start SessionSnapshot. Decoding into a
	// local struct avoids a dependency on hostmux from this package.
	var snap struct {
		SessionID  string              `json:"session_id"`
		ExternalID string              `json:"external_id"`
		Status     agent.SessionStatus `json:"status"`
	}
	p, err := b.path("/start")
	if err != nil {
		return err
	}
	if err := b.c.do(ctx, http.MethodPost, p, req, &snap); err != nil {
		return err
	}
	b.mu.Lock()
	b.externalID = snap.ExternalID
	if snap.Status != "" {
		b.status = snap.Status
	}
	b.mu.Unlock()
	return nil
}

func (b *httpSessionBackend) Watch(ctx context.Context) error {
	p, err := b.path("/watch")
	if err != nil {
		return err
	}
	return b.c.do(ctx, http.MethodPost, p, nil, nil)
}

func (b *httpSessionBackend) SendMessage(ctx context.Context, opts agent.SendMessageOpts) error {
	p, err := b.path("/message")
	if err != nil {
		return err
	}
	return b.c.do(ctx, http.MethodPost, p, opts, nil)
}

func (b *httpSessionBackend) Abort(ctx context.Context) error {
	p, err := b.path("/abort")
	if err != nil {
		return err
	}
	return b.c.do(ctx, http.MethodPost, p, nil, nil)
}

// Stop releases the session on the host. Maps to POST /sessions/{id}/stop.
func (b *httpSessionBackend) Stop() error {
	// Use a background context — Stop is invoked during shutdown when the
	// caller's context may already be cancelled. The HTTP write itself
	// is fast.
	p, err := b.path("/stop")
	if err != nil {
		return err
	}
	return b.c.do(context.Background(), http.MethodPost, p, nil, nil)
}

func (b *httpSessionBackend) Status() agent.SessionStatus {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.status
}

func (b *httpSessionBackend) SessionID() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.externalID
}

func (b *httpSessionBackend) Messages(ctx context.Context) ([]agent.MessageData, error) {
	var out []agent.MessageData
	p, err := b.path("/messages")
	if err != nil {
		return nil, err
	}
	return out, b.c.do(ctx, http.MethodGet, p, nil, &out)
}

func (b *httpSessionBackend) Revert(ctx context.Context, messageID string) error {
	body := struct {
		MessageID string `json:"message_id"`
	}{messageID}
	p, err := b.path("/revert")
	if err != nil {
		return err
	}
	return b.c.do(ctx, http.MethodPost, p, body, nil)
}

func (b *httpSessionBackend) Fork(ctx context.Context, messageID string) (agent.ForkResult, error) {
	body := struct {
		MessageID string `json:"message_id"`
	}{messageID}
	var out agent.ForkResult
	p, err := b.path("/fork")
	if err != nil {
		return out, err
	}
	return out, b.c.do(ctx, http.MethodPost, p, body, &out)
}

func (b *httpSessionBackend) RespondPermission(ctx context.Context, permissionID string, allow bool) error {
	body := struct {
		Allow bool `json:"allow"`
	}{allow}
	p, err := b.path("/permissions/" + url.PathEscape(permissionID) + "/reply")
	if err != nil {
		return err
	}
	return b.c.do(ctx, http.MethodPost, p, body, nil)
}

// Events returns a channel of agent.Event values translated from the
// host's SSE stream. The first call starts a background goroutine that
// holds the SSE response open and pumps events into the channel; the
// channel is closed when the SSE stream ends (host stop, network
// error, etc).
//
// Subsequent calls return the same channel. Callers are expected to
// invoke Events() at most once meaningfully — the daemon's session
// manager already enforces this for the in-process case.
func (b *httpSessionBackend) Events() <-chan agent.Event {
	b.eventsOnce.Do(func() {
		b.eventsCh = make(chan agent.Event, 64)
		go b.streamEvents()
	})
	return b.eventsCh
}

// streamEvents holds open the SSE GET /sessions/{id}/events stream and
// pumps decoded events into b.eventsCh. Closes the channel on any exit.
//
// Uses context.Background because the SSE stream's lifetime is tied to
// the backend's existence, not any single caller's request context. The
// stream terminates when the host closes it (Stop, EOF) or when the
// HTTP client transport is torn down via HTTP.Close.
func (b *httpSessionBackend) streamEvents() {
	defer close(b.eventsCh)

	ctx := context.Background()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.c.baseURL+"/sessions/"+b.sessionID+"/events", nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := b.c.httpc.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}

	parseSSE(resp.Body, func(eventType string, data []byte) {
		if eventType == "end" {
			return
		}
		var ev agent.Event
		if err := json.Unmarshal(data, &ev); err != nil {
			return
		}
		// Cache status updates so Status() reflects the latest known
		// state without a separate HTTP fetch.
		if ev.Type == agent.EventStatusChange {
			if d, ok := ev.Data.(agent.StatusChangeData); ok {
				b.mu.Lock()
				b.status = d.NewStatus
				b.mu.Unlock()
			}
		}
		b.eventsCh <- ev
	})
}

// parseSSE reads a Server-Sent Events stream from r and invokes onEvent
// for each completed event. It is intentionally minimal: it understands
// only `event:` and `data:` field prefixes; multi-line data values are
// joined with newlines per the SSE spec.
func parseSSE(r io.Reader, onEvent func(eventType string, data []byte)) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var (
		evType string
		dataB  []byte
	)
	flush := func() {
		if evType == "" && len(dataB) == 0 {
			return
		}
		onEvent(evType, dataB)
		evType = ""
		dataB = nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			flush()
			continue
		}
		switch {
		case strings.HasPrefix(line, "event:"):
			evType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			payload := strings.TrimPrefix(line, "data:")
			payload = strings.TrimPrefix(payload, " ")
			if dataB != nil {
				dataB = append(dataB, '\n')
			}
			dataB = append(dataB, []byte(payload)...)
		case strings.HasPrefix(line, ":"):
			// Comment line; ignore.
		default:
			// Unknown line; ignore for forward-compat.
		}
	}
	// Trailing event without blank-line terminator.
	flush()
}
