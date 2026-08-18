package hostclient

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/supaclank/clank/internal/agent"
	"github.com/supaclank/clank/internal/host"
)

// The "unsupported" wire code must round-trip back to agent.ErrUnsupported
// so TUI/CLI callers can errors.Is on both the in-process and HTTP paths.
func TestErrorFromResp_Unsupported(t *testing.T) {
	t.Parallel()

	resp := &http.Response{
		StatusCode: http.StatusNotImplemented,
		Status:     "501 Not Implemented",
		Body:       io.NopCloser(strings.NewReader(`{"code":"unsupported","error":"fork is not supported"}`)),
	}
	err := errorFromResp(resp)
	if !errors.Is(err, agent.ErrUnsupported) {
		t.Fatalf("errorFromResp = %v, want errors.Is(_, agent.ErrUnsupported)", err)
	}
}

func TestErrorFromResp_BackendNotAllowed(t *testing.T) {
	t.Parallel()

	resp := &http.Response{
		StatusCode: http.StatusForbidden,
		Status:     "403 Forbidden",
		Body:       io.NopCloser(strings.NewReader(`{"code":"harness_not_allowed","error":"agent harness not allowed"}`)),
	}
	err := errorFromResp(resp)
	if !errors.Is(err, host.ErrBackendNotAllowed) {
		t.Fatalf("errorFromResp = %v, want errors.Is(_, host.ErrBackendNotAllowed)", err)
	}
}
