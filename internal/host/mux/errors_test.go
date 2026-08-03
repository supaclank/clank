package hostmux

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/supaclank/clank/internal/agent"
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
