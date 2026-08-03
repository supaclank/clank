package daemonclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/supaclank/clank/internal/cloud"
	"github.com/supaclank/clank/internal/config"
)

// startFakeIdP spins up a httptest server that mounts the OAuth surface
// EnsureFreshActiveRemote calls: GET /auth-config (discovery) and
// POST /token (refresh exchange). The token handler is configurable so
// individual tests can simulate success, 401, or 5xx.
//
// Returns the server (with cleanup wired to t.Cleanup) so tests can
// pass the URL into preferences as the active remote's gateway_url.
func startFakeIdP(t *testing.T, tokenHandler http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/auth-config", func(w http.ResponseWriter, r *http.Request) {
		// Point token_endpoint back at the same server so the refresh
		// stays self-contained — no need to thread two URLs into prefs.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"authorize_endpoint": srv.URL + "/authorize",
			"token_endpoint":     srv.URL + "/token",
			"client_id":          "test-client",
		})
	})
	mux.HandleFunc("/token", tokenHandler)
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// seedRemote initialises CLANK_DIR with a preferences.json containing a
// single named remote. Caller passes the *Remote so each test owns the
// AccessToken / RefreshToken / ExpiresAt combination it exercises.
func seedRemote(t *testing.T, name string, r *config.Remote) {
	t.Helper()
	t.Setenv("CLANK_DIR", t.TempDir())
	if err := config.SavePreferences(config.Preferences{
		Remote: &config.RemoteConfig{
			Active:   name,
			Profiles: map[string]*config.Remote{name: r},
		},
	}); err != nil {
		t.Fatalf("seed prefs: %v", err)
	}
}

// loadRemote re-reads the active remote so tests can assert on
// post-refresh state.
func loadRemote(t *testing.T) *config.Remote {
	t.Helper()
	prefs, err := config.LoadPreferences()
	if err != nil {
		t.Fatalf("reload prefs: %v", err)
	}
	return prefs.ActiveRemote()
}

func TestEnsureFreshActiveRemote_NoActiveRemote_NoOp(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir())
	if err := EnsureFreshActiveRemote(context.Background()); err != nil {
		t.Fatalf("expected nil for unconfigured prefs, got %v", err)
	}
}

func TestEnsureFreshActiveRemote_StaticBearer_NoOp(t *testing.T) {
	// Static-bearer profiles must never hit the IdP — they have no
	// refresh_token and no expiry. Fail the test if the fake IdP
	// receives any traffic.
	srv := startFakeIdP(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("static-bearer profile should not have called /token")
		http.Error(w, "should not be called", http.StatusInternalServerError)
	})
	seedRemote(t, "dev", &config.Remote{
		GatewayURL:  srv.URL,
		AccessToken: "static-token",
	})
	if err := EnsureFreshActiveRemote(context.Background()); err != nil {
		t.Fatalf("EnsureFreshActiveRemote: %v", err)
	}
	// Tokens unchanged.
	r := loadRemote(t)
	if r.AccessToken != "static-token" {
		t.Errorf("AccessToken got %q, want unchanged 'static-token'", r.AccessToken)
	}
}

func TestEnsureFreshActiveRemote_FreshToken_NoOp(t *testing.T) {
	srv := startFakeIdP(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("fresh token should not have called /token")
	})
	seedRemote(t, "dev", &config.Remote{
		GatewayURL:   srv.URL,
		AccessToken:  "current",
		RefreshToken: "rt-1",
		ExpiresAt:    time.Now().Add(2 * time.Hour).Unix(), // well past grace
	})
	if err := EnsureFreshActiveRemote(context.Background()); err != nil {
		t.Fatalf("EnsureFreshActiveRemote: %v", err)
	}
	if r := loadRemote(t); r.AccessToken != "current" {
		t.Errorf("AccessToken got %q, want unchanged 'current'", r.AccessToken)
	}
}

func TestEnsureFreshActiveRemote_NoRefreshToken_NoOp(t *testing.T) {
	srv := startFakeIdP(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("missing refresh_token should not have called /token")
	})
	seedRemote(t, "dev", &config.Remote{
		GatewayURL:  srv.URL,
		AccessToken: "current",
		ExpiresAt:   1, // long since expired
	})
	if err := EnsureFreshActiveRemote(context.Background()); err != nil {
		t.Fatalf("EnsureFreshActiveRemote: %v", err)
	}
}

func TestEnsureFreshActiveRemote_Expired_RefreshesAndPersists(t *testing.T) {
	const newAccessToken = "fresh-access-token"
	const newRefreshToken = "fresh-refresh-token"
	const expiresIn = int64(3600)
	srv := startFakeIdP(t, func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Method, http.MethodPost; got != want {
			t.Errorf("token method got %q, want %q", got, want)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if got, want := r.Form.Get("grant_type"), "refresh_token"; got != want {
			t.Errorf("grant_type got %q, want %q", got, want)
		}
		if got, want := r.Form.Get("refresh_token"), "stale-rt"; got != want {
			t.Errorf("refresh_token got %q, want %q", got, want)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{"access_token":%q,"refresh_token":%q,"token_type":"Bearer","expires_in":%d}`,
			newAccessToken, newRefreshToken, expiresIn)))
	})
	seedRemote(t, "dev", &config.Remote{
		GatewayURL:   srv.URL,
		AccessToken:  "stale-access",
		RefreshToken: "stale-rt",
		UserEmail:    "user@example.com",
		ExpiresAt:    1, // expired
	})
	before := time.Now().Unix()
	if err := EnsureFreshActiveRemote(context.Background()); err != nil {
		t.Fatalf("EnsureFreshActiveRemote: %v", err)
	}
	got := loadRemote(t)
	if got.AccessToken != newAccessToken {
		t.Errorf("AccessToken got %q, want %q", got.AccessToken, newAccessToken)
	}
	if got.RefreshToken != newRefreshToken {
		t.Errorf("RefreshToken got %q, want %q", got.RefreshToken, newRefreshToken)
	}
	// ExpiresAt must move into the future. Allow small jitter since the
	// server reports expires_in relative to its clock.
	if got.ExpiresAt < before+expiresIn-5 {
		t.Errorf("ExpiresAt got %d, want >= %d", got.ExpiresAt, before+expiresIn-5)
	}
	// Identity preserved when IdP omits it (opaque tokens).
	if got.UserEmail != "user@example.com" {
		t.Errorf("UserEmail got %q, want preserved 'user@example.com'", got.UserEmail)
	}
}

func TestEnsureFreshActiveRemote_RefreshUnauthorized(t *testing.T) {
	srv := startFakeIdP(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh token revoked"}`))
	})
	seedRemote(t, "dev", &config.Remote{
		GatewayURL:   srv.URL,
		AccessToken:  "stale-access",
		RefreshToken: "revoked-rt",
		ExpiresAt:    1,
	})
	err := EnsureFreshActiveRemote(context.Background())
	if err == nil {
		t.Fatal("expected error when IdP rejects refresh, got nil")
	}
	if !errors.Is(err, cloud.ErrUnauthorized) {
		t.Errorf("expected errors.Is(err, cloud.ErrUnauthorized); got %v", err)
	}
	// Stale tokens must remain on disk so a subsequent `clank login`
	// can replace them deterministically.
	if r := loadRemote(t); r.AccessToken != "stale-access" {
		t.Errorf("AccessToken got %q, want preserved 'stale-access'", r.AccessToken)
	}
}

func TestEnsureFreshActiveRemote_RefreshTransientError(t *testing.T) {
	srv := startFakeIdP(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	seedRemote(t, "dev", &config.Remote{
		GatewayURL:   srv.URL,
		AccessToken:  "stale-access",
		RefreshToken: "rt-1",
		ExpiresAt:    1,
	})
	err := EnsureFreshActiveRemote(context.Background())
	if err == nil {
		t.Fatal("expected error for gateway 500, got nil")
	}
	if errors.Is(err, cloud.ErrUnauthorized) {
		t.Errorf("a 500 must not be classified as ErrUnauthorized; got %v", err)
	}
	if !strings.Contains(err.Error(), "refresh access token") {
		t.Errorf("error %q should mention 'refresh access token' for diagnostics", err)
	}
}

// TestEnsureFreshActiveRemote_FallbackActivePersistsToCorrectKey: when
// Remote.Active is empty (legacy prefs, hand-edited file), the resolved
// profile comes from the alphabetically-first-key fallback. A refresh
// must persist back to THAT key, not to "" — otherwise the refreshed
// tokens land in a phantom "" profile and the real one keeps its stale
// state forever.
func TestEnsureFreshActiveRemote_FallbackActivePersistsToCorrectKey(t *testing.T) {
	const newAccessToken = "fresh-access-token"
	srv := startFakeIdP(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{"access_token":%q,"token_type":"Bearer","expires_in":3600}`, newAccessToken)))
	})
	// Active is empty — exercises the fallback path in ActiveProfileAndName.
	t.Setenv("CLANK_DIR", t.TempDir())
	if err := config.SavePreferences(config.Preferences{
		Remote: &config.RemoteConfig{
			Active: "",
			Profiles: map[string]*config.Remote{
				"dev": {
					GatewayURL:   srv.URL,
					AccessToken:  "stale-access",
					RefreshToken: "stale-rt",
					ExpiresAt:    1, // expired
				},
			},
		},
	}); err != nil {
		t.Fatalf("seed prefs: %v", err)
	}

	if err := EnsureFreshActiveRemote(context.Background()); err != nil {
		t.Fatalf("EnsureFreshActiveRemote: %v", err)
	}

	prefs, err := config.LoadPreferences()
	if err != nil {
		t.Fatalf("reload prefs: %v", err)
	}
	if got, ok := prefs.Remote.Profiles[""]; ok {
		t.Errorf("phantom \"\" profile was created: %+v", got)
	}
	dev := prefs.Remote.Profiles["dev"]
	if dev == nil {
		t.Fatal("dev profile vanished")
	}
	if dev.AccessToken != newAccessToken {
		t.Errorf("dev.AccessToken got %q, want %q", dev.AccessToken, newAccessToken)
	}
}
