package gateway

import (
	"net/http"
	"strings"
	"time"

	"github.com/supaclank/clank/pkg/preview/tokens"
)

const (
	previewLauncherSeenCookieName  = "clank_launcher_seen"
	previewLauncherSeenCookieValue = "1"
	previewLauncherSeenCookieTTL   = 365 * 24 * time.Hour
)

func (s *previewState) hasSeenLauncher(r *http.Request) bool {
	cookie, err := r.Cookie(previewLauncherSeenCookieName)
	return err == nil && cookie.Value == previewLauncherSeenCookieValue
}

func (s *previewState) serveLauncherSeen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     previewLauncherSeenCookieName,
		Value:    previewLauncherSeenCookieValue,
		Path:     "/",
		Domain:   strings.TrimPrefix(s.root, "."),
		Expires:  s.now().Add(previewLauncherSeenCookieTTL),
		MaxAge:   int(previewLauncherSeenCookieTTL / time.Second),
		HttpOnly: true,
		Secure:   tokens.RequestIsHTTPS(r),
		SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}
