package host

import "testing"

func TestHostLocalConstant(t *testing.T) {
	t.Parallel()
	// Sanity check: the canonical local host ID should not change without
	// updating callers (TUI defaults, doc references).
	if HostLocal != "local" {
		t.Fatalf("HostLocal changed: got %q, want \"local\"", HostLocal)
	}
}
