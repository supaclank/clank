package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestDeviceFlow_AutoApproves walks the stub through the device-flow
// shape the laptop's internal/cloud client expects: start → poll →
// /me with the returned bearer. JWT signing/verifying primitives are
// tested in pkg/auth.
func TestDeviceFlow_AutoApproves(t *testing.T) {
	t.Parallel()
	cfg := config{
		listen:   ":0",
		secret:   []byte("test-secret"),
		userID:   "dev-user",
		email:    "dev@clank.local",
		tokenTTL: time.Minute,
	}
	s := newServer(cfg)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	cfg.publicURL = srv.URL
	s.cfg = cfg

	// start
	resp, err := http.Post(srv.URL+"/auth/device/start", "application/json", nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start status %d", resp.StatusCode)
	}
	var start struct {
		DeviceCode string `json:"device_code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&start); err != nil {
		t.Fatal(err)
	}
	if start.DeviceCode == "" {
		t.Fatal("empty device_code")
	}

	// poll
	body, _ := json.Marshal(map[string]string{"device_code": start.DeviceCode})
	resp, err = http.Post(srv.URL+"/auth/device/poll", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("poll status %d", resp.StatusCode)
	}
	var token struct {
		AccessToken string `json:"access_token"`
		UserID      string `json:"user_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&token); err != nil {
		t.Fatal(err)
	}
	if token.AccessToken == "" {
		t.Fatal("empty access_token")
	}
	if token.UserID != "dev-user" {
		t.Errorf("user_id: got %q, want dev-user", token.UserID)
	}

	// /me with the bearer
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/me", nil)
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("me status %d", resp.StatusCode)
	}

	// second poll with same code fails (single-use)
	resp, err = http.Post(srv.URL+"/auth/device/poll", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("second poll with same code should fail")
	}
}
