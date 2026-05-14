package daemonclient_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	daemonclient "github.com/acksell/clank/internal/daemonclient"
)

// TestMaterializeMigration_LargeResponseDoesNotSilentlyTruncate is
// the regression test for the 4 KiB io.LimitReader cap that
// silently truncated materialize responses once SessionBlobURLs
// pushed payloads past 4 KiB. The symptom was an opaque
// "decode materialize response: unexpected end of JSON input"
// — io.ReadAll on a saturated LimitReader returns happily, so
// the truncation isn't visible until JSON parsing fails.
//
// We seed a response body that's 8 KiB of valid JSON (well past
// the old cap, well under the new 1 MiB cap) and assert it
// round-trips cleanly. Were the cap to regress, json.Unmarshal
// would error here.
func TestMaterializeMigration_LargeResponseDoesNotSilentlyTruncate(t *testing.T) {
	t.Parallel()

	// Construct a materialize response with enough session URLs to
	// blow well past 4 KiB but stay tiny relative to the new cap.
	urls := make(map[string]string, 20)
	for i := 0; i < 20; i++ {
		// 400 chars per URL × 20 entries ≈ 8 KiB.
		urls["01HSESS"+strings.Repeat("X", 22)+itoa(i)] = "https://example.invalid/" + strings.Repeat("a", 380)
	}
	body, _ := json.Marshal(map[string]any{
		"checkpoint_id":        "ck-large",
		"head_commit":          "deadbeef",
		"manifest_url":         "https://example.invalid/m",
		"head_commit_url":      "https://example.invalid/h",
		"incremental_url":      "https://example.invalid/i",
		"session_manifest_url": "https://example.invalid/sm",
		"session_blob_urls":    urls,
		"migration_token":     "token",
		"migration_expiry":    9999999999,
	})
	if len(body) <= 4<<10 {
		t.Fatalf("test fixture is %d bytes; should exceed the old 4 KiB cap", len(body))
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	c := daemonclient.NewTCPClient(srv.URL, "")
	res, err := c.MaterializeMigration(context.Background(), "wt-large")
	if err != nil {
		t.Fatalf("MaterializeMigration: %v", err)
	}
	if len(res.SessionBlobURLs) != 20 {
		t.Errorf("got %d session URLs, want 20 (truncation regressed?)", len(res.SessionBlobURLs))
	}
	if res.MigrationToken != "token" {
		t.Errorf("MigrationToken = %q, want %q (truncation regressed?)", res.MigrationToken, "token")
	}
}

// TestMaterializeMigration_OversizedResponseErrorsLoudly proves
// that if the daemon EVER sends a body larger than the cap, we
// get a clear error rather than silent corruption. Catches
// future regressions of the silent-truncation class without
// having to actually write a 1 MiB+ response.
func TestMaterializeMigration_OversizedResponseErrorsLoudly(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Stream more than the 1 MiB cap. We don't have to write
		// valid JSON: the cap-check fires before the parser sees it.
		buf := make([]byte, 32<<10)
		for i := 0; i < 40; i++ { // 40 × 32 KiB = 1.25 MiB > cap
			_, _ = w.Write(buf)
		}
	}))
	t.Cleanup(srv.Close)

	c := daemonclient.NewTCPClient(srv.URL, "")
	_, err := c.MaterializeMigration(context.Background(), "wt-oversized")
	if err == nil {
		t.Fatal("expected error when response exceeds the cap")
	}
	if !strings.Contains(err.Error(), "exceeds") && !strings.Contains(err.Error(), "truncate") {
		t.Errorf("error should mention the cap; got: %v", err)
	}
}

// itoa is a tiny base-10 formatter so the test doesn't pull in
// strconv just for the loop label.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	out := []byte{}
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	return string(out)
}
