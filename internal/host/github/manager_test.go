package github

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestUserAgentTransport_DoesNotMutateRequest pins the
// net/http.RoundTripper contract: RoundTrip must not modify the
// caller's *http.Request. The injection has to happen on a clone.
func TestUserAgentTransport_DoesNotMutateRequest(t *testing.T) {
	t.Parallel()

	var seenUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenUA = r.Header.Get("User-Agent")
	}))
	t.Cleanup(srv.Close)

	transport := &userAgentTransport{base: http.DefaultTransport}
	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("User-Agent"); got != "" {
		t.Fatalf("test bug: req should not have UA preset, got %q", got)
	}

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	resp.Body.Close()

	if seenUA != userAgent {
		t.Errorf("server saw UA = %q, want %q", seenUA, userAgent)
	}
	if got := req.Header.Get("User-Agent"); got != "" {
		t.Errorf("RoundTrip mutated caller's req.Header: User-Agent = %q, want empty", got)
	}
}

// TestUserAgentTransport_PreservesExistingUA proves that a caller-set
// UA wins and is unchanged on the way out. Guards against accidentally
// overwriting an explicit upstream UA (e.g. a future composed
// transport that wraps ours).
func TestUserAgentTransport_PreservesExistingUA(t *testing.T) {
	t.Parallel()

	var seenUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenUA = r.Header.Get("User-Agent")
	}))
	t.Cleanup(srv.Close)

	transport := &userAgentTransport{base: http.DefaultTransport}
	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	const callerUA = "caller/1.0"
	req.Header.Set("User-Agent", callerUA)

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	resp.Body.Close()

	if seenUA != callerUA {
		t.Errorf("server saw UA = %q, want %q", seenUA, callerUA)
	}
}
