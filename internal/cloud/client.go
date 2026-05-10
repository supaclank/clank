// Package cloud is the laptop's HTTP client for a cloud control plane
// deployment. It speaks the OAuth 2.0 Device Authorization Grant
// (RFC 8628) so clank stays provider-agnostic — the cloud's user-auth
// mechanism (Supabase today, anything tomorrow) is invisible to clank.
//
// Designed for the TUI's Cloud panel (internal/tui/cloudview.go):
// every call is a one-shot Context-bounded request. No background
// goroutines, no caching: the TUI owns lifecycle and polling cadence.
package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Standard RFC 8628 polling errors. Returned as typed errors so the
// TUI can branch without string matching.
var (
	// ErrAuthorizationPending: user hasn't approved the device yet.
	// Caller continues polling at the agreed interval.
	ErrAuthorizationPending = errors.New("authorization_pending")

	// ErrSlowDown: poll interval was too tight. Caller should add 5s
	// to its interval (per RFC 8628 §3.5) and continue.
	ErrSlowDown = errors.New("slow_down")

	// ErrAccessDenied: the user explicitly denied the request.
	ErrAccessDenied = errors.New("access_denied")

	// ErrExpiredToken: device_code expired before approval. Caller
	// should restart the flow.
	ErrExpiredToken = errors.New("expired_token")
)

// ErrUnauthorized is returned when the bearer is rejected on a
// post-grant request (e.g. /me). Caller should re-prompt sign-in.
var ErrUnauthorized = errors.New("cloud: unauthorized")

// ErrNotSignedUp is returned by Me when the auth token verifies but
// no cloud-side users row exists yet — the user has authenticated with
// the cloud but hasn't claimed a handle.
var ErrNotSignedUp = errors.New("cloud: user not signed up")

// Client wraps an *http.Client with the device-flow + /me endpoints.
type Client struct {
	cloudURL string
	http     *http.Client
}

// New constructs a Client targeting the given cloud base URL (e.g.
// "https://your-cloud.example.com"). httpClient may be nil for the default.
func New(cloudURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{cloudURL: strings.TrimRight(cloudURL, "/"), http: httpClient}
}

// DeviceStartResponse is RFC 8628 §3.2's "Device Authorization
// Response" shape. The cloud generates the codes; clank shows
// UserCode + VerificationURI to the user, then polls with DeviceCode.
type DeviceStartResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// StartDeviceFlow asks the cloud to mint a (device_code, user_code)
// pair. Unauthenticated — anyone can start a flow; the user_code is
// the unguessable hand-off the user types in their browser.
func (c *Client) StartDeviceFlow(ctx context.Context) (*DeviceStartResponse, error) {
	url := c.cloudURL + "/auth/device/start"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device/start: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("device/start: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out DeviceStartResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parse device/start: %w", err)
	}
	return &out, nil
}

// Session is the credential set returned after a successful
// device-flow grant. ExpiresAt is unix-seconds; treat AccessToken as
// invalid once time.Now().Unix() > ExpiresAt.
type Session struct {
	AccessToken  string
	RefreshToken string
	UserID       string
	UserEmail    string
	ExpiresAt    int64
}

type devicePollSuccess struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	UserID       string `json:"user_id"`
	Email        string `json:"email"`
}

type devicePollError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
}

// PollDeviceFlow polls the cloud for the user's approval. Returns the
// session on success, or one of the typed RFC 8628 errors above. Any
// other error is a transport / unexpected response failure.
//
// Caller is responsible for honoring the interval and backing off on
// ErrSlowDown.
func (c *Client) PollDeviceFlow(ctx context.Context, deviceCode string) (*Session, error) {
	url := c.cloudURL + "/auth/device/poll"
	body, _ := json.Marshal(map[string]string{"device_code": deviceCode})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device/poll: %w", err)
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode == http.StatusOK {
		var ok devicePollSuccess
		if err := json.Unmarshal(bodyBytes, &ok); err != nil {
			return nil, fmt.Errorf("parse poll success: %w", err)
		}
		expires := int64(0)
		if ok.ExpiresIn > 0 {
			expires = time.Now().Unix() + ok.ExpiresIn
		}
		return &Session{
			AccessToken:  ok.AccessToken,
			RefreshToken: ok.RefreshToken,
			UserID:       ok.UserID,
			UserEmail:    ok.Email,
			ExpiresAt:    expires,
		}, nil
	}

	// RFC 8628 §3.5: while pending, the server returns 400 with the
	// error code in the JSON body. We treat any 4xx with a recognized
	// error code as one of the typed sentinels.
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		var perr devicePollError
		_ = json.Unmarshal(bodyBytes, &perr)
		switch perr.Error {
		case "authorization_pending":
			return nil, ErrAuthorizationPending
		case "slow_down":
			return nil, ErrSlowDown
		case "access_denied":
			return nil, ErrAccessDenied
		case "expired_token":
			return nil, ErrExpiredToken
		}
		return nil, fmt.Errorf("device/poll: status %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}
	return nil, fmt.Errorf("device/poll: status %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
}

// MeResponse mirrors the cloud's GET /me payload.
type MeResponse struct {
	UserID        string             `json:"user_id"`
	Email         string             `json:"email"`
	Organisations []OrganisationView `json:"organisations"`
	Hubs          []HubView          `json:"hubs"`
	Hosts         []HostView         `json:"hosts"`
}

type OrganisationView struct {
	ID         string `json:"id"`
	Slug       string `json:"slug"`
	Name       string `json:"name"`
	Region     string `json:"region"`
	FlyAppName string `json:"fly_app_name"`
	PlanID     string `json:"plan_id"`
	Role       string `json:"role"`
}

type HubView struct {
	ID        string `json:"id"`
	OrgID     string `json:"org_id"`
	Subdomain string `json:"subdomain"`
	Region    string `json:"region"`
	Status    string `json:"status"`
	PublicURL string `json:"public_url"`
}

type HostView struct {
	ID         string `json:"id"`
	Provider   string `json:"provider"`
	Hostname   string `json:"hostname"`
	Status     string `json:"status"`
	ExternalID string `json:"external_id,omitempty"`
}

// Me fetches the authenticated user's cloud-side state. Returns
// ErrUnauthorized on 401 (token expired) and ErrNotSignedUp on 404.
func (c *Client) Me(ctx context.Context, accessToken string) (*MeResponse, error) {
	url := c.cloudURL + "/me"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cloud /me: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrUnauthorized
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotSignedUp
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("cloud /me: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var me MeResponse
	if err := json.Unmarshal(body, &me); err != nil {
		return nil, fmt.Errorf("parse /me: %w", err)
	}
	return &me, nil
}

// SignOut is best-effort — caller has already cleared the local
// session; this revokes server-side. Errors are swallowed (the user
// has already signed out from their perspective).
func (c *Client) SignOut(ctx context.Context, accessToken string) {
	url := c.cloudURL + "/auth/signout"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := c.http.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}
