package cloud

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFetchAuthConfig_Roundtrip pins the wire format clank reads from
// the gateway's /auth-config — the same JSON shape that pkg/gateway
// emits via AuthConfigHandler.
func TestFetchAuthConfig_Roundtrip(t *testing.T) {
	t.Parallel()
	payload := AuthConfig{
		AuthorizeEndpoint: "https://idp.example.com/authorize",
		TokenEndpoint:     "https://idp.example.com/token",
		ClientID:          "supaclank-cli",
		Scopes:            []string{"openid", "email"},
		DefaultProvider:   "github",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth-config", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := New(srv.URL, nil)
	got, err := c.FetchAuthConfig(context.Background())
	if err != nil {
		t.Fatalf("FetchAuthConfig: %v", err)
	}
	if got.AuthorizeEndpoint != payload.AuthorizeEndpoint {
		t.Errorf("AuthorizeEndpoint: got %q, want %q", got.AuthorizeEndpoint, payload.AuthorizeEndpoint)
	}
	if got.TokenEndpoint != payload.TokenEndpoint {
		t.Errorf("TokenEndpoint: got %q, want %q", got.TokenEndpoint, payload.TokenEndpoint)
	}
	if got.ClientID != payload.ClientID {
		t.Errorf("ClientID: got %q, want %q", got.ClientID, payload.ClientID)
	}
	if len(got.Scopes) != 2 || got.Scopes[0] != "openid" || got.Scopes[1] != "email" {
		t.Errorf("Scopes: got %v, want [openid email]", got.Scopes)
	}
	if got.DefaultProvider != "github" {
		t.Errorf("DefaultProvider: got %q, want github", got.DefaultProvider)
	}
}

// TestFetchAuthConfig_RejectsMissingFields pins that responses missing
// the core endpoints fail validation rather than producing an
// unusable OAuthClient.
func TestFetchAuthConfig_RejectsMissingFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
	}{
		{"missing authorize_endpoint", `{"token_endpoint":"t","client_id":"c"}`},
		{"missing token_endpoint", `{"authorize_endpoint":"a","client_id":"c"}`},
		{"missing client_id", `{"authorize_endpoint":"a","token_endpoint":"t"}`},
		{"empty body", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("GET /auth-config", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			})
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			c := New(srv.URL, nil)
			_, err := c.FetchAuthConfig(context.Background())
			if err == nil || !strings.Contains(err.Error(), "missing") {
				t.Errorf("expected missing-field error, got %v", err)
			}
		})
	}
}
