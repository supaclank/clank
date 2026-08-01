package daemonclient

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNamedPreviewUsesNameAcrossLifecycle(t *testing.T) {
	t.Parallel()

	type observedRequest struct {
		Method string
		Path   string
		Query  string
		Name   string
	}
	requests := make(chan observedRequest, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		var selection struct {
			Name string `json:"name"`
		}
		if len(body) != 0 {
			if err := json.Unmarshal(body, &selection); err != nil {
				t.Errorf("decode selection: %v", err)
			}
		}
		requests <- observedRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query().Get("name"), Name: selection.Name}

		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/worktrees/wt/preview/stop" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path == "/worktrees/wt/preview/logs" {
			_, _ = w.Write([]byte("ready"))
			return
		}
		_, _ = w.Write([]byte(`{"available":true,"service_name":"admin","state":"ready","port":5173}`))
	}))
	t.Cleanup(srv.Close)

	pv := NewTCPClient(srv.URL, "").Preview("wt").Named("admin")
	if _, err := pv.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := pv.Status(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := pv.Logs(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := pv.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}

	want := []observedRequest{
		{Method: http.MethodPost, Path: "/worktrees/wt/preview/start", Name: "admin"},
		{Method: http.MethodGet, Path: "/worktrees/wt/preview/status", Query: "admin"},
		{Method: http.MethodGet, Path: "/worktrees/wt/preview/logs", Query: "admin"},
		{Method: http.MethodPost, Path: "/worktrees/wt/preview/stop", Name: "admin"},
	}
	for i, expected := range want {
		if got := <-requests; got != expected {
			t.Errorf("request %d = %+v, want %+v", i, got, expected)
		}
	}
}
