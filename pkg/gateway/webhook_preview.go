// webhook_preview.go — sprite-facing webhook endpoints that mint and
// revoke tokenized preview URLs.
//
// Auth model: identical to /webhooks/notifications. clank-host on the
// sprite carries its per-host notifier_token bearer (provisioner-issued
// at host create time, stored in `hosts.notifier_token`); the handler
// resolves it to a hosts row, and the row's user_id becomes the
// preview route's owner_user_id. Mounted PRE-auth via
// PreviewWebhookHandler so the user-JWT middleware doesn't 401 the
// host call before it ever reaches us.
//
// register is idempotent on (host_id, worktree_id, service_name) so a
// sprite restart returns the same token — mobile's cached URL doesn't
// churn. revoke is best-effort idempotent so the sprite's preview/stop
// can call it unconditionally without the gateway 404'ing.
package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/acksell/clank/pkg/auth"
	"github.com/acksell/clank/pkg/preview/routestore"
	"github.com/acksell/clank/pkg/preview/tokens"
	"github.com/acksell/clank/pkg/provisioner/hoststore"
)

// previewRegisterRequest is the JSON body for /webhooks/preview/register.
// internal_port is the OS-allocated port Metro is listening on inside
// the sprite; the gateway tunnels here through Sprites' WSS proxy.
type previewRegisterRequest struct {
	WorktreeID   string `json:"worktree_id"`
	ServiceName  string `json:"service_name"`
	InternalPort int    `json:"internal_port"`
}

// previewRegisterResponse mirrors the register row back to the sprite.
// expires_at is plumbed all the way to mobile so the UI can warn about
// expiring tokens (future); url is what the sprite returns in its
// /preview/start response so mobile knows where to fetch the bundle.
type previewRegisterResponse struct {
	Token      string            `json:"token"`
	URL        string            `json:"url"`
	Visibility tokens.Visibility `json:"visibility"`
	ExpiresAt  time.Time         `json:"expires_at"`
}

func (g *Gateway) handlePreviewWebhookRegister(w http.ResponseWriter, r *http.Request) {
	host, ok := g.authPreviewWebhook(w, r)
	if !ok {
		return
	}

	var req previewRegisterRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.WorktreeID == "" {
		http.Error(w, "worktree_id is required", http.StatusBadRequest)
		return
	}
	if req.ServiceName == "" {
		http.Error(w, "service_name is required", http.StatusBadRequest)
		return
	}
	if req.InternalPort < 1 || req.InternalPort > 65535 {
		http.Error(w, "internal_port must be 1..65535", http.StatusBadRequest)
		return
	}

	freshToken, err := tokens.New()
	if err != nil {
		g.log.Printf("preview register: mint token: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	expiresAt := time.Now().Add(tokens.DefaultTokenTTL)
	route, err := g.cfg.PreviewRoutes.Upsert(r.Context(), routestore.Route{
		Token:        freshToken, // ignored on conflict — existing token wins
		OwnerUserID:  host.UserID,
		HostID:       host.ID,
		WorktreeID:   req.WorktreeID,
		ServiceName:  req.ServiceName,
		InternalPort: req.InternalPort,
		Visibility:   tokens.VisibilityOwnerOnly,
		ExpiresAt:    expiresAt,
	})
	if err != nil {
		g.log.Printf("preview register: upsert: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	resp := previewRegisterResponse{
		Token:      route.Token,
		URL:        tokens.URLFor(route.Token, g.cfg.PreviewRootDomain, tokens.SchemeFromRequest(r), tokens.PortFromHost(r.Host)),
		Visibility: route.Visibility,
		ExpiresAt:  route.ExpiresAt,
	}
	writeJSON(w, http.StatusOK, resp)
}

type previewRevokeRequest struct {
	WorktreeID  string `json:"worktree_id"`
	ServiceName string `json:"service_name"`
}

func (g *Gateway) handlePreviewWebhookRevoke(w http.ResponseWriter, r *http.Request) {
	host, ok := g.authPreviewWebhook(w, r)
	if !ok {
		return
	}

	var req previewRevokeRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.WorktreeID == "" {
		http.Error(w, "worktree_id is required", http.StatusBadRequest)
		return
	}
	if req.ServiceName == "" {
		http.Error(w, "service_name is required", http.StatusBadRequest)
		return
	}
	if err := g.cfg.PreviewRoutes.RevokeByService(r.Context(), host.ID, req.WorktreeID, req.ServiceName); err != nil {
		g.log.Printf("preview revoke: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// authPreviewWebhook performs the bearer-resolve dance and returns the
// resolved host on success. On any failure it writes an error response
// and returns ok=false; the caller MUST return immediately.
//
// The error responses match dispatcher.Handle's shape (plain-text,
// matching status codes) so an operator reading both routes' logs
// sees a uniform vocabulary.
func (g *Gateway) authPreviewWebhook(w http.ResponseWriter, r *http.Request) (hoststore.Host, bool) {
	token, err := auth.ExtractBearer(r)
	if err != nil {
		http.Error(w, "missing bearer", http.StatusUnauthorized)
		return hoststore.Host{}, false
	}
	host, err := g.cfg.PreviewHostLookup.GetHostByNotifierToken(r.Context(), token)
	if err != nil {
		if errors.Is(err, hoststore.ErrHostNotFound) {
			http.Error(w, "unknown notifier token", http.StatusUnauthorized)
			return hoststore.Host{}, false
		}
		g.log.Printf("preview webhook host lookup: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return hoststore.Host{}, false
	}
	return host, true
}

// writeJSON is declared in pkg/gateway/responses.go.
