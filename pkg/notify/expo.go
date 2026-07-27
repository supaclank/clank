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
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
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

	// codeTooManyExperienceIDs is Expo's whole-batch rejection when the
	// tokens in one request span multiple experiences (projects) — e.g.
	// a user with both the dev and prod app installed. Expo requires
	// every message in a request to target the same project, and its
	// error details map experience ID → offending tokens, which is the
	// only place that mapping exists (tokens don't encode it).
	codeTooManyExperienceIDs = "PUSH_TOO_MANY_EXPERIENCE_IDS"

	// MismatchedExperienceID is a clankd-synthesized ticket error — not
	// an Expo code. It marks a token that Expo attributed to a different
	// experience than the one this client is pinned to (WithExperienceID):
	// undeliverable from this deployment, so the dispatcher purges it.
	MismatchedExperienceID = "MismatchedExperienceId"
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

// IsMismatchedExperience reports whether the ticket marks a token that
// belongs to a different Expo experience than this client sends for
// (see MismatchedExperienceID). Callers purge these like dead tokens.
func (t Ticket) IsMismatchedExperience() bool {
	return t.Status == "error" && t.Details.Error == MismatchedExperienceID
}

// IsUndeliverable reports whether the token can never receive a push
// from this deployment, so its device row should be purged rather than
// retried. This is the provider-agnostic predicate the dispatcher acts
// on; which error codes qualify (a dead token, a token pinned out by
// WithExperienceID) is Expo-specific detail that stays here. A future
// Pusher impl decides its own permanent-failure codes behind the same
// method.
func (t Ticket) IsUndeliverable() bool {
	return t.IsDeviceNotRegistered() || t.IsMismatchedExperience()
}

// Client is the Expo Push API client. Construct with New.
type Client struct {
	endpoint     string
	accessToken  string
	experienceID string
	http         *http.Client
	log          *log.Logger
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
//
//	notify.New(lg).WithAccessToken(os.Getenv("EXPO_ACCESS_TOKEN"))
func (c *Client) WithAccessToken(token string) *Client {
	c.accessToken = token
	return c
}

// WithExperienceID pins the client to one Expo experience (project),
// e.g. "@supaclank/clank". When a mixed batch gets split per experience
// (see pushPerExperience), tokens Expo attributes to any other
// experience are not re-sent; they come back as MismatchedExperienceId
// tickets so the dispatcher purges their device rows. Empty (default)
// re-sends every group — right for self-hosters who don't know which
// app builds their users run.
//
// Chainable, like WithAccessToken. Production supplies it via env
// (e.g. EXPO_EXPERIENCE_ID).
func (c *Client) WithExperienceID(id string) *Client {
	c.experienceID = id
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
		tickets, err := c.pushBatchSplitting(ctx, msgs[start:end])
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
	Data   []Ticket  `json:"data"`
	Errors []expoErr `json:"errors,omitempty"`
}

type expoErr struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Details is code-specific; for PUSH_TOO_MANY_EXPERIENCE_IDS it is
	// a map of experience ID → tokens. RawMessage because other codes
	// use other shapes.
	Details json.RawMessage `json:"details,omitempty"`
}

// tooManyExperiencesError is pushBatch's typed form of Expo's
// PUSH_TOO_MANY_EXPERIENCE_IDS rejection. groups carries Expo's own
// attribution of the batch tokens to experience IDs, which
// pushPerExperience uses to split and retry.
type tooManyExperiencesError struct {
	groups map[string][]string
}

func (e *tooManyExperiencesError) Error() string {
	return fmt.Sprintf("expo error: %s — batch spans %d experience IDs", codeTooManyExperienceIDs, len(e.groups))
}

// asTooManyExperiences extracts a tooManyExperiencesError from Expo's
// top-level errors, or nil when the failure is something else (or the
// details don't parse — then the generic error path handles it).
func asTooManyExperiences(errs []expoErr) *tooManyExperiencesError {
	for _, e := range errs {
		if e.Code != codeTooManyExperienceIDs {
			continue
		}
		var groups map[string][]string
		if err := json.Unmarshal(e.Details, &groups); err != nil || len(groups) == 0 {
			continue
		}
		return &tooManyExperiencesError{groups: groups}
	}
	return nil
}

// pushBatchSplitting is pushBatch plus recovery from mixed-experience
// batches: Expo rejects those wholesale, so without the split a single
// foreign token (a dev install next to a prod one) blackholes every
// notification for the user.
func (c *Client) pushBatchSplitting(ctx context.Context, batch []Message) ([]Ticket, error) {
	tickets, err := c.pushBatch(ctx, batch)
	var tme *tooManyExperiencesError
	if !errors.As(err, &tme) {
		return tickets, err
	}
	return c.pushPerExperience(ctx, batch, tme.groups)
}

// pushPerExperience re-sends a rejected mixed batch as one request per
// experience ID, using Expo's error details as the token→experience
// mapping (grouping can't happen upfront — the tokens themselves don't
// encode the experience). Tickets come back in the original batch
// order. When the client is pinned (WithExperienceID), foreign groups
// are dropped with MismatchedExperienceId tickets instead of re-sent.
// A sub-batch that still fails yields error tickets for its own
// messages only, so one bad group doesn't kill delivery to the rest.
func (c *Client) pushPerExperience(ctx context.Context, batch []Message, groups map[string][]string) ([]Ticket, error) {
	expByToken := make(map[string]string)
	for exp, tokens := range groups {
		for _, tok := range tokens {
			expByToken[tok] = exp
		}
	}
	// Partition by experience, remembering original positions. Tokens
	// Expo didn't attribute (shouldn't happen) share the "" group and
	// still get sent rather than silently dropped.
	idxsByExp := map[string][]int{}
	for i, m := range batch {
		exp := expByToken[m.To]
		idxsByExp[exp] = append(idxsByExp[exp], i)
	}
	exps := make([]string, 0, len(idxsByExp))
	for exp := range idxsByExp {
		exps = append(exps, exp)
	}
	sort.Strings(exps)

	tickets := make([]Ticket, len(batch))
	for _, exp := range exps {
		idxs := idxsByExp[exp]
		if c.experienceID != "" && exp != "" && exp != c.experienceID {
			c.log.Printf("dropping %d token(s) for experience %s (pinned to %s)", len(idxs), exp, c.experienceID)
			for _, i := range idxs {
				t := Ticket{Status: "error", Message: fmt.Sprintf("token belongs to experience %s, not %s", exp, c.experienceID)}
				t.Details.Error = MismatchedExperienceID
				tickets[i] = t
			}
			continue
		}
		sub := make([]Message, len(idxs))
		for j, i := range idxs {
			sub[j] = batch[i]
		}
		subTickets, err := c.pushBatch(ctx, sub)
		if err == nil && len(subTickets) != len(sub) {
			err = fmt.Errorf("expected %d tickets, got %d", len(sub), len(subTickets))
		}
		if err != nil {
			c.log.Printf("push sub-batch (experience %q, %d msgs): %v", exp, len(sub), err)
			for _, i := range idxs {
				tickets[i] = Ticket{Status: "error", Message: err.Error()}
			}
			continue
		}
		for j, i := range idxs {
			tickets[i] = subTickets[j]
		}
	}
	return tickets, nil
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
		// Expo returns structured errors with 4xx statuses (the mixed-
		// experience rejection is a 400) — read enough body to parse
		// them, not just a log snippet.
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var er expoResponse
		if json.Unmarshal(raw, &er) == nil {
			if tme := asTooManyExperiences(er.Errors); tme != nil {
				return nil, tme
			}
			if len(er.Errors) > 0 {
				return nil, fmt.Errorf("expo error: %s — %s", er.Errors[0].Code, er.Errors[0].Message)
			}
		}
		if len(raw) > 512 {
			raw = raw[:512]
		}
		return nil, fmt.Errorf("expo POST: status %d body=%q", resp.StatusCode, raw)
	}
	var er expoResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if tme := asTooManyExperiences(er.Errors); tme != nil {
		return nil, tme
	}
	if len(er.Errors) > 0 {
		// Top-level errors are whole-batch failures (e.g. invalid JSON
		// shape, throttling). Surface the first one — Expo guarantees
		// at most one in practice.
		return nil, fmt.Errorf("expo error: %s — %s", er.Errors[0].Code, er.Errors[0].Message)
	}
	return er.Data, nil
}
