package tokens

import (
	"net/http"
	"strconv"
	"time"
)

const (
	// EmbedParam selects a preview embedded in the owner's browser workspace.
	EmbedParam        = "__clank_embed"
	embeddedSigCookie = "__Host-clank_embed_sig"
	embeddedExpCookie = "__Host-clank_embed_exp"

	embeddedParamValue = "1"
	secFetchDestHeader = "Sec-Fetch-Dest"
	secFetchDestIframe = "iframe"
)

// SetEmbeddedSignedCookies authenticates subresources inside a cross-site iframe.
// Partitioning isolates these credentials to the embedding top-level site.
func SetEmbeddedSignedCookies(w http.ResponseWriter, sig string, exp time.Time) {
	maxAge := max(0, int(time.Until(exp).Seconds()))
	for name, value := range map[string]string{embeddedSigCookie: sig, embeddedExpCookie: strconv.FormatInt(exp.Unix(), 10)} {
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: value, Path: "/", MaxAge: maxAge, Expires: exp,
			Secure: true, HttpOnly: true, SameSite: http.SameSiteNoneMode, Partitioned: true,
		})
	}
}

// SetRequestSignedCookies chooses the browser credential transport for this request.
func SetRequestSignedCookies(w http.ResponseWriter, r *http.Request, sig string, exp time.Time) {
	if IsEmbeddedRequest(r) && RequestIsHTTPS(r) {
		SetEmbeddedSignedCookies(w, sig, exp)
		return
	}
	SetSignedCookies(w, sig, exp, RequestIsHTTPS(r))
}

// IsEmbeddedRequest reports a preview loaded inside another page.
func IsEmbeddedRequest(r *http.Request) bool {
	return r.URL.Query().Get(EmbedParam) == embeddedParamValue || r.Header.Get(secFetchDestHeader) == secFetchDestIframe
}
