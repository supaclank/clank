package flyio

import (
	"context"
	"testing"

	"github.com/acksell/clank/internal/agent"
)

// The probe/install decision table is exercised exhaustively through
// ensureOpenCodeInstalledOn (opencode_install_test.go) — both
// wrappers share ensureAgentCLIInstalledOn. These tests pin the
// claude-specific wiring: the wrapper compares against
// agent.PinnedClaudeVersion, not the opencode pin.

// TestEnsureClaude_PinnedVersionSkipsInstall: a probe reporting the
// claude pin (already parsed to the bare version by the probe
// closure) never touches the installer.
func TestEnsureClaude_PinnedVersionSkipsInstall(t *testing.T) {
	t.Parallel()
	stubs := &opencodeStubs{probeVersion: agent.PinnedClaudeVersion}
	p := newTestProvisioner(t)
	reinstalled, err := p.ensureClaudeInstalledOn(context.Background(), "s1", stubs.probe, newStubFS(), stubs.install)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if reinstalled {
		t.Error("pinned-version match should report reinstalled=false")
	}
	if stubs.installCalled {
		t.Error("install must not run when the probe matches the pinned version")
	}
}

// TestEnsureClaude_VersionDriftReinstalls: an image-baked claude at a
// stale version is positive evidence — install runs. 2.1.168 is the
// literal version the 2026-07-05 sprite was frozen at.
func TestEnsureClaude_VersionDriftReinstalls(t *testing.T) {
	t.Parallel()
	stubs := &opencodeStubs{probeVersion: "2.1.168"}
	p := newTestProvisioner(t)
	reinstalled, err := p.ensureClaudeInstalledOn(context.Background(), "s1", stubs.probe, newStubFS(), stubs.install)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !reinstalled || !stubs.installCalled {
		t.Errorf("version drift should reinstall; reinstalled=%v installCalled=%v", reinstalled, stubs.installCalled)
	}
}

// TestEnsureClaude_OpencodePinDoesNotMatch guards against the two
// wrappers sharing a pin by accident: a probe reporting the OPENCODE
// pinned version through the claude wrapper must still reinstall.
func TestEnsureClaude_OpencodePinDoesNotMatch(t *testing.T) {
	t.Parallel()
	stubs := &opencodeStubs{probeVersion: agent.PinnedOpencodeVersion}
	p := newTestProvisioner(t)
	reinstalled, err := p.ensureClaudeInstalledOn(context.Background(), "s1", stubs.probe, newStubFS(), stubs.install)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !reinstalled || !stubs.installCalled {
		t.Errorf("opencode's pin must not satisfy the claude wrapper; reinstalled=%v installCalled=%v", reinstalled, stubs.installCalled)
	}
}
