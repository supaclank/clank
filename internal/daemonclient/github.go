package daemonclient

// Daemon-client bindings for the GitHub Connect surface exposed by
// the gateway. All calls route through the local daemon's /v1/github
// proxies, which forward to the user's host (laptop's clank-host
// subprocess in the local case; sprite in the cloud case).
//
// Shapes mirror internal/host/github and pkg/gateway/github_*.go
// one-for-one — duplicated here so daemonclient stays decoupled from
// those packages' types (same pattern as WorktreeInfo above).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// GitHubStatus mirrors github.Status — see internal/host/github/manager.go.
type GitHubStatus struct {
	Available   bool      `json:"available"`
	Connected   bool      `json:"connected"`
	GitHubLogin string    `json:"github_login,omitempty"`
	Scopes      []string  `json:"scopes,omitempty"`
	InstalledAt time.Time `json:"installed_at,omitzero"`
}

// GitHubDeviceFlowStart mirrors github.DeviceFlowStart.
type GitHubDeviceFlowStart struct {
	FlowID                  string    `json:"flow_id"`
	UserCode                string    `json:"user_code"`
	VerificationURI         string    `json:"verification_uri"`
	VerificationURIComplete string    `json:"verification_uri_complete"`
	ExpiresAt               time.Time `json:"expires_at"`
	Interval                int       `json:"interval"`
}

// GitHubDeviceFlowState matches github.DeviceFlowState.
type GitHubDeviceFlowState string

const (
	GitHubFlowPending  GitHubDeviceFlowState = "pending"
	GitHubFlowSuccess  GitHubDeviceFlowState = "success"
	GitHubFlowDenied   GitHubDeviceFlowState = "denied"
	GitHubFlowExpired  GitHubDeviceFlowState = "expired"
	GitHubFlowError    GitHubDeviceFlowState = "error"
	GitHubFlowCanceled GitHubDeviceFlowState = "canceled"
)

// GitHubDeviceFlowStatus is the polled state of a connect flow.
type GitHubDeviceFlowStatus struct {
	State       GitHubDeviceFlowState `json:"state"`
	Error       string                `json:"error,omitempty"`
	GitHubLogin string                `json:"github_login,omitempty"`
}

// GitHubCreatePRRequest is the body of POST /v1/worktrees/{id}/pr.
// All fields except Draft are required (host enforces this).
type GitHubCreatePRRequest struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Base  string `json:"base"`
	Draft bool   `json:"draft"`
}

// GitHubCreatePRResponse is the 201 body of the PR creation route.
type GitHubCreatePRResponse struct {
	PRNumber   int    `json:"pr_number"`
	PRURL      string `json:"pr_url"`
	HeadBranch string `json:"head_branch"`
	BaseBranch string `json:"base_branch"`
	HeadSHA    string `json:"head_sha"`
}

// GitHubPRAlreadyExistsError carries the existing-PR URL the gateway
// returns alongside the 409 branch_already_has_pr response so the CLI
// can render it as a "View existing PR" link.
type GitHubPRAlreadyExistsError struct {
	ExistingURL string
	Message     string
}

func (e *GitHubPRAlreadyExistsError) Error() string {
	if e.ExistingURL != "" {
		return fmt.Sprintf("%s (existing: %s)", e.Message, e.ExistingURL)
	}
	return e.Message
}

// GitHubStatus fetches the host's GitHub-connect status.
func (c *Client) GitHubStatus(ctx context.Context) (GitHubStatus, error) {
	var out GitHubStatus
	if err := c.githubGet(ctx, "/v1/github/status", &out); err != nil {
		return GitHubStatus{}, err
	}
	return out, nil
}

// GitHubDisconnect removes the host's stored GitHub credentials.
// Idempotent — missing credentials is not an error.
func (c *Client) GitHubDisconnect(ctx context.Context) error {
	return c.githubDo(ctx, http.MethodDelete, "/v1/github", nil, nil)
}

// GitHubConnectStart kicks off the device flow on the host. Cancels
// any prior in-flight flow first (single-slot registry on the host).
func (c *Client) GitHubConnectStart(ctx context.Context) (GitHubDeviceFlowStart, error) {
	var out GitHubDeviceFlowStart
	if err := c.githubDo(ctx, http.MethodPost, "/v1/github/connect/start", nil, &out); err != nil {
		return GitHubDeviceFlowStart{}, err
	}
	return out, nil
}

// GitHubConnectStatus polls one flow. ErrGitHubUnknownFlow when the
// id doesn't match the host's active slot.
func (c *Client) GitHubConnectStatus(ctx context.Context, flowID string) (GitHubDeviceFlowStatus, error) {
	var out GitHubDeviceFlowStatus
	path := "/v1/github/connect/status?flow_id=" + url.QueryEscape(flowID)
	if err := c.githubGet(ctx, path, &out); err != nil {
		return GitHubDeviceFlowStatus{}, err
	}
	return out, nil
}

// GitHubConnectCancel signals the host to abort an in-flight flow.
// No-op for a flow that's already terminal.
func (c *Client) GitHubConnectCancel(ctx context.Context, flowID string) error {
	path := "/v1/github/connect/cancel?flow_id=" + url.QueryEscape(flowID)
	return c.githubDo(ctx, http.MethodPost, path, nil, nil)
}

// GitHubCreatePR pushes the worktree's branch and opens a PR. Returns
// a typed *GitHubPRAlreadyExistsError when a PR for the head branch
// already exists, with the existing URL extracted from the 409 body.
func (c *Client) GitHubCreatePR(ctx context.Context, worktreeID string, req GitHubCreatePRRequest) (GitHubCreatePRResponse, error) {
	var out GitHubCreatePRResponse
	path := "/v1/worktrees/" + url.PathEscape(worktreeID) + "/pr"
	if err := c.githubDo(ctx, http.MethodPost, path, req, &out); err != nil {
		return GitHubCreatePRResponse{}, err
	}
	return out, nil
}

// --- internal: HTTP plumbing ---

func (c *Client) githubGet(ctx context.Context, path string, out any) error {
	return c.githubDo(ctx, http.MethodGet, path, nil, out)
}

func (c *Client) githubDo(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode body: %w", err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
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
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return classifyGitHubErr(resp.StatusCode, respBody)
	}
	if resp.StatusCode == http.StatusNoContent || out == nil {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// ErrGitHubUnknownFlow corresponds to the gateway's 404 unknown_flow.
var ErrGitHubUnknownFlow = errors.New("github: unknown flow id")

// classifyGitHubErr decodes the gateway's structured error body and
// promotes the cases the CLI needs to special-case (already-exists,
// unknown-flow) into typed errors.
func classifyGitHubErr(status int, body []byte) error {
	var er struct {
		Code        string `json:"code"`
		Error       string `json:"error"`
		ExistingURL string `json:"existing_url,omitempty"`
	}
	_ = json.Unmarshal(body, &er)
	switch er.Code {
	case "branch_already_has_pr":
		return &GitHubPRAlreadyExistsError{
			ExistingURL: er.ExistingURL,
			Message:     er.Error,
		}
	case "unknown_flow":
		return ErrGitHubUnknownFlow
	}
	if er.Error != "" {
		return fmt.Errorf("github (HTTP %d): %s", status, er.Error)
	}
	return fmt.Errorf("github HTTP %d", status)
}
