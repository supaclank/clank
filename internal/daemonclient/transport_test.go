package daemonclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSummarizeBody_HTMLExtractsTitle is the headline regression
// test: when the daemon (or the upstream sprite edge / cloudflare
// tunnel) returns a multi-KB HTML error page, the TUI used to dump
// the entire HTML — stylesheet and all — into its error banner.
// summarizeBody should extract just the <title>.
func TestSummarizeBody_HTMLExtractsTitle(t *testing.T) {
	t.Parallel()
	body := []byte(`<!DOCTYPE html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <title>404 | Sprites</title>
    <style>* { box-sizing: border-box; }</style>
  </head>
  <body>not found</body>
</html>`)
	got := summarizeBody("text/html; charset=utf-8", body)
	if got != "404 | Sprites" {
		t.Errorf("HTML title extraction: got %q, want %q", got, "404 | Sprites")
	}
}

func TestSummarizeBody_HTMLWithoutTitleFallsBackToTrunc(t *testing.T) {
	t.Parallel()
	body := []byte(`<html><body>some markup without a title tag</body></html>`)
	got := summarizeBody("text/html", body)
	if got == "" {
		t.Error("expected fallback summary, got empty")
	}
	if strings.Contains(got, "<title") {
		t.Error("fallback should not include <title> markup")
	}
}

func TestSummarizeBody_PlainTextCollapsesWhitespace(t *testing.T) {
	t.Parallel()
	body := []byte("line one\n\n  line  two\t\ttabbed   out\n")
	got := summarizeBody("text/plain", body)
	want := "line one line two tabbed out"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestSummarizeBody_TruncatesLongPayloads pins the hard 240-char cap
// so a giant text response can't blow the inbox banner up.
func TestSummarizeBody_TruncatesLongPayloads(t *testing.T) {
	t.Parallel()
	body := []byte(strings.Repeat("a", 1000))
	got := summarizeBody("text/plain", body)
	if len(got) > 250 {
		t.Errorf("summary length %d exceeds reasonable cap (~240)", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected truncation marker, got tail %q", got[len(got)-10:])
	}
}

// errServer returns an httptest server that answers every request
// with the given status and body.
func errServer(t *testing.T, status int, contentType, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestDo_StructuredErrorReturnsAPIError pins the typed-error contract:
// a {code, error} body surfaces as *APIError carrying the code, while
// Error() keeps the historical "daemon: <msg>" shape so existing
// string-matching callers see no change.
func TestDo_StructuredErrorReturnsAPIError(t *testing.T) {
	t.Parallel()
	srv := errServer(t, http.StatusNotFound, "application/json",
		`{"code":"not_running","error":"preview: no dev server is running"}`)

	err := NewTCPClient(srv.URL, "").Preview("01WT").Stop(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if apiErr.Code != "not_running" || apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("APIError = %+v, want code=not_running status=404", apiErr)
	}
	if got, want := err.Error(), "daemon: preview: no dev server is running"; got != want {
		t.Errorf("Error() = %q, want %q (historical shape)", got, want)
	}
}

// TestPreviewStart_NoPreviewMapsToErrNotPreviewable pins the sentinel
// promotion the CLI's Expo/Vite hint gates on.
func TestPreviewStart_NoPreviewMapsToErrNotPreviewable(t *testing.T) {
	t.Parallel()
	srv := errServer(t, http.StatusNotFound, "application/json",
		`{"code":"no_preview","error":"preview: worktree is not previewable"}`)

	_, err := NewTCPClient(srv.URL, "").Preview("01WT").Start(context.Background())
	if !errors.Is(err, ErrNotPreviewable) {
		t.Fatalf("want ErrNotPreviewable, got %v", err)
	}
}

// TestDo_NonJSONErrorKeepsStatusSummary pins the fallback for upstream
// proxies that answer with plain text (502s from a dead host, etc).
func TestDo_NonJSONErrorKeepsStatusSummary(t *testing.T) {
	t.Parallel()
	srv := errServer(t, http.StatusBadGateway, "text/plain", "Bad Gateway")

	err := NewTCPClient(srv.URL, "").Preview("01WT").Stop(context.Background())
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Fatalf("non-JSON body must not produce an APIError, got %+v", apiErr)
	}
	if err == nil || !strings.Contains(err.Error(), "daemon returned status 502") {
		t.Errorf("err = %v, want status-summary fallback", err)
	}
}

// TestPreviewLogs_StructuredErrorReturnsAPIError pins Logs' error path
// to the same {code,error} contract as do() — it used its own raw
// request path and previously dropped the body, surfacing only
// "daemon returned status N".
func TestPreviewLogs_StructuredErrorReturnsAPIError(t *testing.T) {
	t.Parallel()
	srv := errServer(t, http.StatusNotFound, "application/json",
		`{"code":"not_running","error":"preview: no dev server is running"}`)

	_, err := NewTCPClient(srv.URL, "").Preview("01WT").Logs(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if apiErr.Code != "not_running" || apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("APIError = %+v, want code=not_running status=404", apiErr)
	}
}

// TestPreviewLogs_NonJSONErrorKeepsStatusSummary mirrors
// TestDo_NonJSONErrorKeepsStatusSummary for Logs' separate request path.
func TestPreviewLogs_NonJSONErrorKeepsStatusSummary(t *testing.T) {
	t.Parallel()
	srv := errServer(t, http.StatusBadGateway, "text/plain", "Bad Gateway")

	_, err := NewTCPClient(srv.URL, "").Preview("01WT").Logs(context.Background())
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Fatalf("non-JSON body must not produce an APIError, got %+v", apiErr)
	}
	if err == nil || !strings.Contains(err.Error(), "daemon returned status 502: Bad Gateway") {
		t.Errorf("err = %v, want status-summary fallback with body", err)
	}
}

func TestHTMLTitle_HandlesAttributesAndCase(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		`<title>Plain</title>`:           "Plain",
		`<TITLE>Upper</TITLE>`:           "Upper",
		`<title id="x">  Spaced  </title>`: "Spaced",
		`<head><title>First</title><title>Second</title></head>`: "First",
		`no title here`:                  "",
	}
	for in, want := range cases {
		if got := htmlTitle([]byte(in)); got != want {
			t.Errorf("htmlTitle(%q) = %q, want %q", in, got, want)
		}
	}
}
