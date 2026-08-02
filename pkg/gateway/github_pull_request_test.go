package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGitHubPullRequestProxyRoutes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		path     string
		hostPath string
	}{
		{name: "inspect", path: "/v1/github/pull-requests/inspect", hostPath: "/github/pull-requests/inspect"},
		{name: "launch", path: "/v1/github/pull-requests/launch", hostPath: "/github/pull-requests/launch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			host := &captureHost{respStatus: http.StatusConflict, respCT: "application/json", respBody: `{"code":"pull_request_changed"}`}
			gw := newGatewayForGitHubProxyTest(t, host)
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(`{"owner":"acme"}`))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			gw.ServeHTTP(rec, req)
			if rec.Code != http.StatusConflict {
				t.Errorf("status = %d, want verbatim 409", rec.Code)
			}
			if got := host.path.Load(); got != tc.hostPath {
				t.Errorf("host path = %v, want %s", got, tc.hostPath)
			}
		})
	}
}
