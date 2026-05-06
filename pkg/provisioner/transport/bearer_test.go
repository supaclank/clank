package transport

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBearerInjector_HostPinSkipsCrossHostRedirect pins the security
// fix CR raised: with Host pinned to the upstream, a cross-host
// redirect followed by http.Client must not leak the bearer to the
// redirect target.
func TestBearerInjector_HostPinSkipsCrossHostRedirect(t *testing.T) {
	t.Parallel()

	// "evil" upstream the redirect points at — records whether the
	// bearer leaked through.
	gotEvilAuth := make(chan string, 1)
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEvilAuth <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(evil.Close)

	// "trusted" upstream — issues a 302 to evil. We pin the bearer
	// to this host's URL.Host.
	gotTrustedAuth := make(chan string, 1)
	trusted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTrustedAuth <- r.Header.Get("Authorization")
		http.Redirect(w, r, evil.URL, http.StatusFound)
	}))
	t.Cleanup(trusted.Close)

	// Parse trusted to get its Host (host:port form).
	trustedHost := strings.TrimPrefix(trusted.URL, "http://")
	cli := &http.Client{Transport: &BearerInjector{
		Token: "secret-token",
		Host:  trustedHost,
	}}

	resp, err := cli.Get(trusted.URL + "/anything")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	if got := <-gotTrustedAuth; got != "Bearer secret-token" {
		t.Errorf("trusted upstream got Authorization=%q, want \"Bearer secret-token\"", got)
	}
	if got := <-gotEvilAuth; got != "" {
		t.Errorf("evil redirect target got Authorization=%q (LEAK); want empty when Host pin doesn't match", got)
	}
}

// TestBearerInjector_EmptyHostInjectsEverywhere preserves the legacy
// behavior for callers that haven't pinned a host yet.
func TestBearerInjector_EmptyHostInjectsEverywhere(t *testing.T) {
	t.Parallel()

	gotAuth := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cli := &http.Client{Transport: &BearerInjector{Token: "tok"}} // no Host
	resp, err := cli.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	if got := <-gotAuth; got != "Bearer tok" {
		t.Errorf("got Authorization=%q, want \"Bearer tok\"", got)
	}
}

// TestBearerInjector_EmptyTokenSkipsHeader: a zero-value token shouldn't
// emit a "Bearer " (space + empty) header.
func TestBearerInjector_EmptyTokenSkipsHeader(t *testing.T) {
	t.Parallel()

	gotAuth := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cli := &http.Client{Transport: &BearerInjector{}} // empty token
	resp, err := cli.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	if got := <-gotAuth; got != "" {
		t.Errorf("got Authorization=%q, want empty", got)
	}
}
