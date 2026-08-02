package daemonclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSignPreviewToken(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.EscapedPath() != "/v1/preview/tokens/tok%2Fwith%2Fslashes/sign" {
			t.Errorf("request = %s %s", r.Method, r.URL.EscapedPath())
		}
		if r.Header.Get("Authorization") != "Bearer owner-token" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		var body struct {
			TTL string `json:"ttl"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.TTL != "24h0m0s" {
			t.Errorf("body = %+v, err = %v", body, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"signed_url":"https://preview.example/?sig=x","expires_at":"2026-08-02T16:00:00Z"}`))
	}))
	t.Cleanup(srv.Close)

	got, err := NewTCPClient(srv.URL, "owner-token").SignPreviewToken(context.Background(), "tok/with/slashes", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got.SignedURL != "https://preview.example/?sig=x" || got.ExpiresAt.IsZero() {
		t.Errorf("signed = %+v", got)
	}
}
