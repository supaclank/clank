// preview_tokens.go — owner-facing token management API.
//
// Routes mounted inside Handler() (inherits outer JWT auth wrap):
//
//	GET    /v1/preview/tokens                — list the caller's live tokens
//	POST   /v1/preview/tokens/{token}/share  — flip visibility + extend TTL
//	DELETE /v1/preview/tokens/{token}        — revoke a specific token
//
// Cross-tenant gate: every mutation re-fetches the route and asserts
// route.OwnerUserID == principal.UserID. A token-guessing attacker
// gets 404 (not 403) so they can't distinguish "not yours" from
// "doesn't exist." The list endpoint returns only the caller's rows.
package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/acksell/clank/pkg/auth"
	"github.com/acksell/clank/pkg/preview/routestore"
	"github.com/acksell/clank/pkg/preview/tokens"
)

// previewTokenView is the JSON shape returned to the owner. Omits
// internal_port (an implementation detail) and owner_user_id (the
// caller already knows). expires_at lets the UI warn about pending
// expiry; visibility drives the share-state toggle in the UI.
type previewTokenView struct {
	Token       string            `json:"token"`
	URL         string            `json:"url"`
	HostID      string            `json:"host_id"`
	WorktreeID  string            `json:"worktree_id"`
	ServiceName string            `json:"service_name"`
	Visibility  tokens.Visibility `json:"visibility"`
	CreatedAt   time.Time         `json:"created_at"`
	ExpiresAt   time.Time         `json:"expires_at"`
}

// viewFor renders a route into the wire shape. scheme + port come
// from the inbound request so URL matches the API origin.
func (g *Gateway) viewFor(r routestore.Route, scheme, port string) previewTokenView {
	return previewTokenView{
		Token:       r.Token,
		URL:         tokens.URLFor(r.Token, g.cfg.PreviewRootDomain, scheme, port),
		HostID:      r.HostID,
		WorktreeID:  r.WorktreeID,
		ServiceName: r.ServiceName,
		Visibility:  r.Visibility,
		CreatedAt:   r.CreatedAt,
		ExpiresAt:   r.ExpiresAt,
	}
}

func (g *Gateway) handleListPreviewTokens(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok || principal.UserID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	rows, err := g.cfg.PreviewRoutes.ListByOwner(r.Context(), principal.UserID)
	if err != nil {
		g.log.Printf("preview list: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	scheme := tokens.SchemeFromRequest(r)
	port := tokens.PortFromHost(r.Host)
	out := make([]previewTokenView, len(rows))
	for i, row := range rows {
		out[i] = g.viewFor(row, scheme, port)
	}
	writeJSON(w, http.StatusOK, out)
}

// shareRequest is the POST body for /v1/preview/tokens/{token}/share.
// TTL is optional; zero or unset means "use the default" — the share
// always extends expires_at to (now + TTL), even toggling back to
// owner_only, so the caller can refresh a stale URL by re-sharing it
// to its current visibility.
type shareRequest struct {
	Visibility tokens.Visibility `json:"visibility"`
	TTL        string            `json:"ttl,omitempty"` // Go duration string, e.g. "1h"
}

type shareResponse struct {
	URL        string            `json:"url"`
	Visibility tokens.Visibility `json:"visibility"`
	ExpiresAt  time.Time         `json:"expires_at"`
}

func (g *Gateway) handleSharePreviewToken(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok || principal.UserID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	token := r.PathValue("token")
	if token == "" {
		http.Error(w, "token is required", http.StatusBadRequest)
		return
	}

	var req shareRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !req.Visibility.Valid() {
		http.Error(w, "invalid visibility (want owner_only or public)", http.StatusBadRequest)
		return
	}
	ttl := tokens.DefaultTokenTTL
	if req.TTL != "" {
		parsed, err := time.ParseDuration(req.TTL)
		if err != nil {
			http.Error(w, "ttl: "+err.Error(), http.StatusBadRequest)
			return
		}
		if parsed <= 0 {
			http.Error(w, "ttl must be positive", http.StatusBadRequest)
			return
		}
		ttl = parsed
	}

	// Ownership check before mutation. GetByToken returns ErrNotFound
	// for revoked/expired rows too, so a stale token gets a clean 404.
	existing, err := g.cfg.PreviewRoutes.GetByToken(r.Context(), token)
	if err != nil {
		if errors.Is(err, routestore.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		g.log.Printf("preview share lookup: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if existing.OwnerUserID != principal.UserID {
		// Deliberate 404 to avoid leaking the existence of someone
		// else's token through a 403.
		http.NotFound(w, r)
		return
	}

	updated, err := g.cfg.PreviewRoutes.SetVisibility(r.Context(), token, req.Visibility, time.Now().Add(ttl))
	if err != nil {
		if errors.Is(err, routestore.ErrNotFound) {
			// Lost the race: revoked between our read and our write.
			http.NotFound(w, r)
			return
		}
		g.log.Printf("preview share update: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, shareResponse{
		URL:        tokens.URLFor(updated.Token, g.cfg.PreviewRootDomain, tokens.SchemeFromRequest(r), tokens.PortFromHost(r.Host)),
		Visibility: updated.Visibility,
		ExpiresAt:  updated.ExpiresAt,
	})
}

// signRequest is the body of POST /v1/preview/tokens/{token}/sign.
// TTL caps to tokens.MaxSigTTL; default tokens.DefaultSigTTL.
type signRequest struct {
	TTL       string `json:"ttl,omitempty"` // Go duration string
	SessionID string `json:"session_id,omitempty"`
	Backend   string `json:"backend,omitempty"`
}

type signResponse struct {
	SignedURL string    `json:"signed_url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// handleSignPreviewToken returns a short-lived signed URL the owner
// can hand to a client that can't carry an Authorization header
// (Expo's dev-launcher, the RN bundle runtime). The signature is
// HMAC'd over (token, exp) with PreviewSigningKey — see
// pkg/preview/tokens/sign.go.
//
// Ownership check mirrors the other token endpoints: the route
// must exist (GetByToken returns ErrNotFound for revoked/expired)
// AND its owner must match the JWT principal. Non-owners get 404,
// not 403, to avoid existence-leak.
func (g *Gateway) handleSignPreviewToken(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok || principal.UserID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	token := r.PathValue("token")
	if token == "" {
		http.Error(w, "token is required", http.StatusBadRequest)
		return
	}

	var req signRequest
	// Empty body is fine — use defaults. ContentLength==-1 means
	// unknown (e.g. chunked encoding); treat that the same as 0 to
	// avoid a spurious 400 on clients that don't set Content-Length.
	if r.ContentLength > 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
			http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	ttl := tokens.DefaultSigTTL
	if req.TTL != "" {
		parsed, err := time.ParseDuration(req.TTL)
		if err != nil {
			http.Error(w, "ttl: "+err.Error(), http.StatusBadRequest)
			return
		}
		if parsed <= 0 {
			http.Error(w, "ttl must be positive", http.StatusBadRequest)
			return
		}
		if parsed > tokens.MaxSigTTL {
			http.Error(w, fmt.Sprintf("ttl exceeds max of %s", tokens.MaxSigTTL), http.StatusBadRequest)
			return
		}
		ttl = parsed
	}

	existing, err := g.cfg.PreviewRoutes.GetByToken(r.Context(), token)
	if err != nil {
		if errors.Is(err, routestore.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		g.log.Printf("preview sign lookup: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if existing.OwnerUserID != principal.UserID {
		// 404 not 403 — same existence-leak guard as share/delete.
		http.NotFound(w, r)
		return
	}

	exp := time.Now().Add(ttl)
	// Scheme + port come from the request so the URL the client
	// follows mirrors its current origin: https on cloud (no port),
	// http://<host>:<port> on local docker dev.
	signedURL, err := tokens.SignedURL(
		g.cfg.PreviewSigningKey, token, g.cfg.PreviewRootDomain,
		tokens.SchemeFromRequest(r), tokens.PortFromHost(r.Host), exp,
	)
	if err != nil {
		g.log.Printf("preview sign: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	signedURL, err = appendPreviewOverlayContext(signedURL, previewOverlayContext{
		SessionID: req.SessionID,
		Backend:   req.Backend,
	})
	if err != nil {
		g.log.Printf("preview sign overlay context: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, signResponse{SignedURL: signedURL, ExpiresAt: exp})
}

func (g *Gateway) handleDeletePreviewToken(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok || principal.UserID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	token := r.PathValue("token")
	if token == "" {
		http.Error(w, "token is required", http.StatusBadRequest)
		return
	}

	existing, err := g.cfg.PreviewRoutes.GetByToken(r.Context(), token)
	if err != nil {
		if errors.Is(err, routestore.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		g.log.Printf("preview delete lookup: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if existing.OwnerUserID != principal.UserID {
		http.NotFound(w, r)
		return
	}

	if err := g.cfg.PreviewRoutes.Revoke(r.Context(), token); err != nil {
		if errors.Is(err, routestore.ErrNotFound) {
			// Already revoked by a concurrent call; treat as success.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		g.log.Printf("preview delete: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
