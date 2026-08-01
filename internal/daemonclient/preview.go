package daemonclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// ErrNotPreviewable is retained for compatibility with older hosts that
// returned no_preview before launch configuration setup was supported.
var ErrNotPreviewable = errors.New("preview: project is not previewable")

// ErrPreviewSetupRequired classifies a missing web launch configuration.
var ErrPreviewSetupRequired = errors.New("preview: launch setup is required")

// PreviewSetupRequiredError carries the connected-agent setup task and both
// supported output paths returned by a current host.
type PreviewSetupRequiredError struct {
	Message           string
	SetupPrompt       string
	ProjectConfigPath string
	HostConfigPath    string
}

func (e *PreviewSetupRequiredError) Error() string {
	return "daemon: " + e.Message
}

func (e *PreviewSetupRequiredError) Unwrap() error {
	return ErrPreviewSetupRequired
}

// codeNoPreview is retained for compatibility with pre-config hosts.
const codeNoPreview = "no_preview"

const codePreviewSetupRequired = "preview_setup_required"

// PreviewClient is the worktree-scoped handle for the Expo/Metro dev-server
// preview lifecycle. Routes proxy through the gateway to the owning host.
type PreviewClient struct {
	c          *Client
	worktreeID string
	name       string
}

// Named returns a handle scoped to one configured preview name.
func (p *PreviewClient) Named(name string) *PreviewClient {
	return &PreviewClient{c: p.c, worktreeID: p.worktreeID, name: name}
}

// Preview returns a handle bound to a worktree's dev-server preview.
func (c *Client) Preview(worktreeID string) *PreviewClient {
	return &PreviewClient{c: c, worktreeID: worktreeID}
}

// PreviewStatus is the dev-server state returned by Start/Status. Port is
// the dev server's listen port — populated even on the laptop path, where
// the gateway-minted public URL/Token fields stay empty. Kind mirrors
// preview.Kind ("expo" | "web") and tells `clank preview` which client
// flow to run (QR + phone vs browser overlay proxy).
type PreviewStatus struct {
	Available         bool   `json:"available"`
	SetupRequired     bool   `json:"setup_required"`
	SetupPrompt       string `json:"setup_prompt"`
	ProjectConfigPath string `json:"project_config_path"`
	HostConfigPath    string `json:"host_config_path"`
	Kind              string `json:"kind"`
	ServiceName       string `json:"service_name"`
	State             string `json:"state"`
	LastError         string `json:"last_err"`
	Port              int    `json:"port"`
	URL               string `json:"url"`
	Token             string `json:"token"`
}

// Start spawns (or returns the existing) dev server for the preview
// key — a managed worktree ID or a folder slug (host.LocalRepoSlug);
// the host resolves both. Idempotent on the host side.
func (p *PreviewClient) Start(ctx context.Context) (*PreviewStatus, error) {
	var s PreviewStatus
	var body any
	if p.name != "" {
		body = struct {
			Name string `json:"name"`
		}{Name: p.name}
	}
	if err := p.c.post(ctx, "/worktrees/"+p.worktreeID+"/preview/start", body, &s); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Code == codeNoPreview {
			return nil, fmt.Errorf("%w: %s", ErrNotPreviewable, apiErr.Message)
		}
		if errors.As(err, &apiErr) && apiErr.Code == codePreviewSetupRequired {
			return nil, &PreviewSetupRequiredError{
				Message:           apiErr.Message,
				SetupPrompt:       apiErr.SetupPrompt,
				ProjectConfigPath: apiErr.ProjectConfigPath,
				HostConfigPath:    apiErr.HostConfigPath,
			}
		}
		return nil, err
	}
	return &s, nil
}

// Status returns availability + running state without spawning.
func (p *PreviewClient) Status(ctx context.Context) (*PreviewStatus, error) {
	var s PreviewStatus
	if err := p.c.get(ctx, p.path("status"), &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Stop terminates the worktree's dev server. Idempotent: a 404
// not_running is surfaced as an error by the transport, so callers that
// want naive idempotency should ignore it.
func (p *PreviewClient) Stop(ctx context.Context) error {
	var body any
	if p.name != "" {
		body = struct {
			Name string `json:"name"`
		}{Name: p.name}
	}
	return p.c.post(ctx, "/worktrees/"+p.worktreeID+"/preview/stop", body, nil)
}

// Logs returns the dev server's captured stdout/stderr tail
// (text/plain, ANSI-stripped host-side; empty when nothing is
// running). Raw request — the shared do() helper assumes JSON bodies.
func (p *PreviewClient) Logs(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		p.c.baseURL+p.path("logs"), nil)
	if err != nil {
		return nil, err
	}
	if p.c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+p.c.authToken)
	}
	resp, err := p.c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("daemon request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		var errResp struct {
			Code              string `json:"code"`
			Error             string `json:"error"`
			SetupPrompt       string `json:"setup_prompt"`
			ProjectConfigPath string `json:"project_config_path"`
			HostConfigPath    string `json:"host_config_path"`
		}
		if json.Unmarshal(body, &errResp) == nil && errResp.Error != "" {
			return nil, &APIError{
				StatusCode:        resp.StatusCode,
				Code:              errResp.Code,
				Message:           errResp.Error,
				SetupPrompt:       errResp.SetupPrompt,
				ProjectConfigPath: errResp.ProjectConfigPath,
				HostConfigPath:    errResp.HostConfigPath,
			}
		}
		return nil, fmt.Errorf("daemon returned status %d: %s", resp.StatusCode, summarizeBody(resp.Header.Get("Content-Type"), body))
	}
	return body, nil
}

func (p *PreviewClient) path(operation string) string {
	path := "/worktrees/" + p.worktreeID + "/preview/" + operation
	if p.name != "" {
		path += "?name=" + url.QueryEscape(p.name)
	}
	return path
}
