package tokens

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEmbeddedSignatureSurvivesSubresourceRequest(t *testing.T) {
	t.Parallel()
	key := []byte("01234567890123456789012345678901")
	expires := time.Now().Add(time.Hour)
	sig, err := Sign(key, "preview-token", expires)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	SetEmbeddedSignedCookies(recorder, sig, expires)
	request := httptest.NewRequest(http.MethodGet, "https://preview.example/app.js", nil)
	for _, cookie := range recorder.Result().Cookies() {
		if !cookie.Partitioned || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteNoneMode || cookie.Domain != "" {
			t.Fatalf("unsafe embedded cookie: %+v", cookie)
		}
		request.AddCookie(cookie)
	}
	if err := VerifyFromRequest(key, "preview-token", request, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := VerifyFromRequest(key, "another-token", request, time.Now()); err == nil {
		t.Fatal("cookie authorized another preview")
	}
}

func TestRequestSignedCookieTransport(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, url, destination string
		partitioned, secure    bool
	}{
		{"board", "https://preview.example/?__clank_embed=1", "", true, true},
		{"iframe", "https://preview.example/", "iframe", true, true},
		{"new tab", "https://preview.example/", "document", false, true},
		{"local", "http://preview.example/?__clank_embed=1", "iframe", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, tc.url, nil)
			r.Header.Set("Sec-Fetch-Dest", tc.destination)
			w := httptest.NewRecorder()
			SetRequestSignedCookies(w, r, "signature", time.Now().Add(time.Hour))
			cookies := w.Result().Cookies()
			if len(cookies) != 2 {
				t.Fatalf("got %d cookies", len(cookies))
			}
			for _, cookie := range cookies {
				if cookie.Partitioned != tc.partitioned || cookie.Secure != tc.secure || !cookie.HttpOnly {
					t.Fatalf("unexpected cookie: %+v", cookie)
				}
				if !tc.partitioned && cookie.SameSite != http.SameSiteStrictMode {
					t.Fatal("top-level cookie lost SameSite protection")
				}
			}
		})
	}
}
