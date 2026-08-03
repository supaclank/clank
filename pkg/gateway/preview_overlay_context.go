package gateway

import (
	"net/http"
	"net/url"
	"time"

	"github.com/supaclank/clank/pkg/preview/tokens"
)

const (
	overlaySessionParam = "clank_overlay_session"
	overlayBackendParam = "clank_overlay_backend"
)

type previewOverlayContext struct {
	SessionID string
	Backend   string
}

func appendPreviewOverlayContext(rawURL string, context previewOverlayContext) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	if context.SessionID != "" {
		q.Set(overlaySessionParam, context.SessionID)
	}
	if context.Backend != "" {
		q.Set(overlayBackendParam, context.Backend)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func previewOverlayContextFromRequest(r *http.Request) previewOverlayContext {
	return previewOverlayContext{
		SessionID: requestParamOrCookie(r, overlaySessionParam),
		Backend:   requestParamOrCookie(r, overlayBackendParam),
	}
}

func requestParamOrCookie(r *http.Request, name string) string {
	if value := r.URL.Query().Get(name); value != "" {
		return value
	}
	cookie, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func setPreviewOverlayContextCookies(
	w http.ResponseWriter,
	r *http.Request,
	expiresAt time.Time,
) {
	secure := tokens.RequestIsHTTPS(r)
	for _, name := range []string{overlaySessionParam, overlayBackendParam} {
		value := r.URL.Query().Get(name)
		cookie := &http.Cookie{
			Name:     name,
			Value:    value,
			Path:     "/",
			Expires:  expiresAt,
			Secure:   secure,
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
		}
		if value == "" {
			cookie.Expires = time.Unix(1, 0)
			cookie.MaxAge = -1
		}
		http.SetCookie(w, cookie)
	}
}
