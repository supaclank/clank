package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/acksell/clank/pkg/auth"
)

// createProjectRequest is what mobile sends to POST /v1/projects/create.
// template is a catalog id (see GET /v1/templates); the gateway resolves
// it to a clone URL — clients never send URLs.
type createProjectRequest struct {
	Template string `json:"template"`
	Name     string `json:"name"`
}

// hostCreateProjectRequest is the body the gateway sends to the host's
// POST /projects/create. Builtin catalog ids resolve to clone_url here
// (the host never sees catalog ids); github: ids forward as a
// github_template ref the HOST resolves with its own credential.
type hostCreateProjectRequest struct {
	CloneURL       string `json:"clone_url,omitempty"`
	GitHubTemplate string `json:"github_template,omitempty"`
	Name           string `json:"name"`
}

// handleCreateProject services POST /v1/projects/create — the mobile
// app's entry point for scaffolding a brand-new project (no existing
// repo). The flow:
//
//  1. Authenticate the caller (Principal in context).
//  2. Resolve the requested template id to a clone URL from the
//     configured catalog. Unknown id → 404; this is the only place
//     template ids are interpreted.
//  3. Proxy to the user's host POST /projects/create with the clone URL
//     and return the host's response. The host scaffolds a bare
//     canonical + linked worktree under ~/work (see
//     internal/host/create_project.go); its filesystem is the registry,
//     so there is nothing to record gateway-side.
//
// Host 4xx responses forward verbatim (typed, client-actionable); host
// 5xx is masked to a flat 502 because the body can carry raw git stderr
// including the resolved template clone URL, which may embed credentials.
func (g *Gateway) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok || principal.UserID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req createProjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Template == "" {
		http.Error(w, "template is required", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	hostReq := hostCreateProjectRequest{Name: req.Name}
	switch {
	case strings.HasPrefix(req.Template, githubTemplateIDPrefix):
		ref, err := githubTemplateRef(req.Template)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		hostReq.GitHubTemplate = ref
	default:
		cloneURL, ok := g.cloneURLForTemplate(req.Template)
		if !ok {
			http.Error(w, fmt.Sprintf("unknown template %q", req.Template), http.StatusNotFound)
			return
		}
		hostReq.CloneURL = cloneURL
	}

	status, body, err := g.callHostCreateProject(r.Context(), principal.UserID, hostReq)
	if err != nil {
		g.log.Printf("gateway create-project: host call: %v", err)
		http.Error(w, "project creation failed", http.StatusBadGateway)
		return
	}
	if status/100 == 4 && status != http.StatusUnauthorized {
		// Forward the host's typed 4xx verbatim (e.g. 400
		// invalid_argument) so the client can react precisely — a flat
		// 502 hid every real cause (the projects_import.go pattern).
		// 401 is excluded: the host's own auth middleware returns it
		// only when the gateway's credentials to the host are rejected
		// (an infra failure, not a client-facing one — the host's
		// application logic uses 403 for github_not_connected etc.), and
		// forwarding it verbatim would falsely signal the client's own
		// gateway session expired.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
		return
	}
	if status/100 != 2 {
		// Host 5xx bodies can carry raw git stderr — including the
		// RESOLVED template clone URL, which may embed credentials — so
		// mask to a generic 502 and keep the body out of logs too.
		g.log.Printf("gateway create-project: host returned %d (%d-byte body withheld, may contain credentials)", status, len(body))
		http.Error(w, "project creation failed", http.StatusBadGateway)
		return
	}

	// Success: forward the host's CreateWorktreeResult verbatim.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// callHostCreateProject makes the POST /projects/create request to the
// user's host. Transport-level failures return err (→ 502); otherwise
// the host's (status, body) come back for the handler to forward.
func (g *Gateway) callHostCreateProject(ctx context.Context, userID string, in hostCreateProjectRequest) (int, []byte, error) {
	ref, err := g.cfg.Provisioner.EnsureHost(ctx, userID)
	if err != nil {
		return 0, nil, fmt.Errorf("ensure host: %w", err)
	}

	buf, err := json.Marshal(in)
	if err != nil {
		return 0, nil, fmt.Errorf("marshal request: %w", err)
	}

	target := strings.TrimRight(ref.URL, "/") + "/projects/create"
	if _, err := url.Parse(target); err != nil {
		return 0, nil, fmt.Errorf("invalid host URL: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(buf))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	// Cloning a template is a network operation on the host; give it more
	// headroom than a plain proxy call.
	cli := &http.Client{Transport: ref.Transport, Timeout: 120 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("post host /projects/create: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, nil
}
