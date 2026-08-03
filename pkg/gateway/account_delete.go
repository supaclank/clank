package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/supaclank/clank/pkg/auth"
	"github.com/supaclank/clank/pkg/preview/routestore"
)

// handleDeleteAccount services DELETE /v1/account: self-service GDPR/app-store
// erasure of the calling principal's entire account. The userID is the
// authenticated caller — a user can only delete themselves, so there is no
// target path param and no cross-tenant surface.
//
// Steps run most-destructive-first and abort on the first error. Every step is
// idempotent, so a client that retries after a 5xx re-runs completed steps as
// no-ops and resumes at the failure. "Nothing to delete" is success (204), not
// 404 — erasing an already-empty account is the desired end state.
func (g *Gateway) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	userID := auth.MustPrincipal(r.Context()).UserID

	// Detach from the request context so a client disconnect doesn't abort a
	// partial erasure mid-way; erasure must complete to satisfy GDPR obligations.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Minute)
	defer cancel()

	// 1. Destroy all compute first — the riskiest (external provider) call,
	// and the one that wipes every sprite-side artifact (sessions, ~/work
	// repos + worktrees, Anthropic creds, GitHub tokens). The sprite's
	// filesystem is the only repo/session store, so this step IS the data
	// purge. Force-destroys regardless of busy sessions: a running agent
	// must not block erasure. A failure here leaves every gateway-side
	// index untouched for a clean retry.
	if err := g.cfg.Provisioner.DestroyHostsByUser(ctx, userID); err != nil {
		g.log.Printf("account delete: DestroyHostsByUser(%s): %v", userID, err)
		http.Error(w, "sprite teardown failed (account left intact — retry): "+err.Error(), http.StatusBadGateway)
		return
	}

	// 2. Delete push-notification devices (optional surface).
	if g.cfg.Notify != nil {
		if err := g.cfg.Notify.DeleteDevicesByUser(ctx, userID); err != nil {
			g.log.Printf("account delete: DeleteDevicesByUser(%s): %v", userID, err)
			http.Error(w, "device cleanup failed (retry): "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	// 3. Revoke preview routes (optional surface).
	if g.cfg.PreviewRoutes != nil {
		if err := g.revokePreviewRoutesForUser(ctx, userID); err != nil {
			g.log.Printf("account delete: revoke preview routes(%s): %v", userID, err)
			http.Error(w, "preview route cleanup failed (retry): "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	// 4. Delete the user from the external IdP last (the external system of
	// record). Optional — nil = clank-data-only deletion. Until the IdP
	// revokes the token, a still-valid JWT could EnsureHost a fresh sprite,
	// so operators who need a hard erasure wire this.
	if g.cfg.IdPDeleter != nil {
		if err := g.cfg.IdPDeleter.DeleteUser(ctx, userID); err != nil {
			g.log.Printf("account delete: IdP DeleteUser(%s): %v", userID, err)
			http.Error(w, "identity-provider deletion failed (retry): "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// revokePreviewRoutesForUser revokes every live preview route the user owns.
// Revoke is idempotent (ErrNotFound on an already-revoked/expired row), which
// is treated as success so a retry never errors on rows a prior attempt cleared.
func (g *Gateway) revokePreviewRoutesForUser(ctx context.Context, userID string) error {
	routes, err := g.cfg.PreviewRoutes.ListByOwner(ctx, userID)
	if err != nil {
		return fmt.Errorf("list preview routes: %w", err)
	}
	for _, rt := range routes {
		if err := g.cfg.PreviewRoutes.Revoke(ctx, rt.Token); err != nil && !errors.Is(err, routestore.ErrNotFound) {
			return fmt.Errorf("revoke route: %w", err)
		}
	}
	return nil
}
