package hostmux

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/supaclank/clank/internal/agent"
	"github.com/supaclank/clank/internal/host"
)

// Regression: unsupported backend operations (e.g. fork on a backend
// without fork) must surface as 501 {code:"unsupported"}, not a generic
// 500, so clients can degrade gracefully.
func TestWriteError_Unsupported(t *testing.T) {
	t.Parallel()

	rr := httptest.NewRecorder()
	writeError(rr, fmt.Errorf("fork: %w", agent.ErrUnsupported))

	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusNotImplemented)
	}
	var e struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if e.Code != "unsupported" {
		t.Errorf("code = %q, want %q", e.Code, "unsupported")
	}
}

func TestWriteError_BackendNotAllowed(t *testing.T) {
	t.Parallel()

	rr := httptest.NewRecorder()
	writeError(rr, fmt.Errorf("list agents: %w", host.ErrBackendNotAllowed))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}
	var e struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if e.Code != "harness_not_allowed" {
		t.Errorf("code = %q, want %q", e.Code, "harness_not_allowed")
	}
}
