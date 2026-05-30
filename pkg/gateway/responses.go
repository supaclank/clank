package gateway

import (
	"encoding/json"
	"errors"
	"net/http"

	clanksync "github.com/acksell/clank/pkg/sync"
)

// syncErrToHTTP maps errors from the sync.Server direct-call API to
// HTTP responses. ErrWorktreeNotFound → 404, ErrOwnerMismatch → 409,
// ErrForbidden → 403; everything else → 502.
func syncErrToHTTP(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, clanksync.ErrWorktreeNotFound):
		http.Error(w, op+": "+err.Error(), http.StatusNotFound)
	case errors.Is(err, clanksync.ErrOwnerMismatch):
		http.Error(w, op+": "+err.Error(), http.StatusConflict)
	case errors.Is(err, clanksync.ErrForbidden):
		http.Error(w, op+": "+err.Error(), http.StatusForbidden)
	default:
		http.Error(w, op+": "+err.Error(), http.StatusBadGateway)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
