package flyio

import (
	"context"
	"errors"
	"io/fs"
	"testing"

	sprites "github.com/superfly/sprites-go"

	"github.com/acksell/clank/internal/agent"
)

// opencodeStubs bundles the injected probe/install hooks and records
// whether the install ran.
type opencodeStubs struct {
	probeVersion  string
	probeErr      error
	installOut    []byte
	installErr    error
	installCalled bool
}

func (s *opencodeStubs) probe(context.Context) (string, error) {
	return s.probeVersion, s.probeErr
}

func (s *opencodeStubs) install(context.Context) ([]byte, error) {
	s.installCalled = true
	return s.installOut, s.installErr
}

// failingStatFS returns the same error for every op — simulates the
// filesystem API being unreachable too.
type failingStatFS struct{ err error }

func (f failingStatFS) Open(string) (fs.File, error)     { return nil, f.err }
func (f failingStatFS) Stat(string) (fs.FileInfo, error) { return nil, f.err }

// TestEnsureOpenCode_PinnedVersionSkipsInstall pins the happy path: a
// successful probe at the pinned version never touches the installer.
func TestEnsureOpenCode_PinnedVersionSkipsInstall(t *testing.T) {
	t.Parallel()
	stubs := &opencodeStubs{probeVersion: agent.PinnedOpencodeVersion}
	p := newTestProvisioner(t)
	reinstalled, err := p.ensureOpenCodeInstalledOn(context.Background(), "s1", stubs.probe, newStubFS(), stubs.install)
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

// TestEnsureOpenCode_VersionDriftReinstalls: probe ran fine but the
// version drifted — positive evidence, install runs.
func TestEnsureOpenCode_VersionDriftReinstalls(t *testing.T) {
	t.Parallel()
	stubs := &opencodeStubs{probeVersion: "0.0.1"}
	p := newTestProvisioner(t)
	reinstalled, err := p.ensureOpenCodeInstalledOn(context.Background(), "s1", stubs.probe, newStubFS(), stubs.install)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !reinstalled || !stubs.installCalled {
		t.Errorf("version drift should reinstall; reinstalled=%v installCalled=%v", reinstalled, stubs.installCalled)
	}
}

// TestEnsureOpenCode_ExitErrorReinstalls: the command executed on the
// sprite and exited non-zero (broken binary / dangling symlink) —
// positive evidence about opencode itself, install runs.
func TestEnsureOpenCode_ExitErrorReinstalls(t *testing.T) {
	t.Parallel()
	stubs := &opencodeStubs{probeErr: &sprites.ExitError{Code: 127}}
	p := newTestProvisioner(t)
	reinstalled, err := p.ensureOpenCodeInstalledOn(context.Background(), "s1", stubs.probe, newStubFS(), stubs.install)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !reinstalled || !stubs.installCalled {
		t.Errorf("sprite-side exit error should reinstall; reinstalled=%v installCalled=%v", reinstalled, stubs.installCalled)
	}
}

// TestEnsureOpenCode_TransportErrorBinaryPresentFailsFast is the
// 2026-07-02 regression: a probe that never ran (wake-race transport
// failure) with opencode present on disk must fail fast — NOT burn
// the 3-minute install deadline on the same dead channel.
func TestEnsureOpenCode_TransportErrorBinaryPresentFailsFast(t *testing.T) {
	t.Parallel()
	probeErr := errors.New("connection closed")
	stubs := &opencodeStubs{probeErr: probeErr}
	stat := newStubFS()
	stat.seed(opencodePath, []byte("elf"))

	p := newTestProvisioner(t)
	reinstalled, err := p.ensureOpenCodeInstalledOn(context.Background(), "s1", stubs.probe, stat, stubs.install)
	if err == nil {
		t.Fatal("expected fail-fast error, got nil")
	}
	if !errors.Is(err, probeErr) {
		t.Errorf("error should wrap the probe error; got %v", err)
	}
	if reinstalled || stubs.installCalled {
		t.Errorf("transport failure with binary present must not install; reinstalled=%v installCalled=%v", reinstalled, stubs.installCalled)
	}
}

// TestEnsureOpenCode_TransportErrorBinaryAbsentInstalls: on a fresh
// sprite the probe can fail for transport-ish reasons too (or because
// the binary is simply absent) — the fs tiebreak says absent, so the
// install must still run or fresh provisioning would break.
func TestEnsureOpenCode_TransportErrorBinaryAbsentInstalls(t *testing.T) {
	t.Parallel()
	stubs := &opencodeStubs{probeErr: context.DeadlineExceeded}
	p := newTestProvisioner(t)
	reinstalled, err := p.ensureOpenCodeInstalledOn(context.Background(), "s1", stubs.probe, newStubFS(), stubs.install)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !reinstalled || !stubs.installCalled {
		t.Errorf("absent binary should install even after a probe failure; reinstalled=%v installCalled=%v", reinstalled, stubs.installCalled)
	}
}

// TestEnsureOpenCode_StatErrorFailsFast: probe AND presence check both
// failed — everything about the channel is suspect, so fail fast
// rather than attempt a blind install.
func TestEnsureOpenCode_StatErrorFailsFast(t *testing.T) {
	t.Parallel()
	probeErr := errors.New("connection closed")
	stubs := &opencodeStubs{probeErr: probeErr}
	p := newTestProvisioner(t)
	reinstalled, err := p.ensureOpenCodeInstalledOn(context.Background(), "s1", stubs.probe, failingStatFS{err: errors.New("fs api unreachable")}, stubs.install)
	if err == nil {
		t.Fatal("expected fail-fast error, got nil")
	}
	if !errors.Is(err, probeErr) {
		t.Errorf("error should wrap the probe error; got %v", err)
	}
	if reinstalled || stubs.installCalled {
		t.Errorf("unverifiable state must not install; reinstalled=%v installCalled=%v", reinstalled, stubs.installCalled)
	}
}

// TestEnsureOpenCode_NilStatFSFailsFast: a nil statFS must fail fast
// with a wrapped error, not panic inside fs.Stat.
func TestEnsureOpenCode_NilStatFSFailsFast(t *testing.T) {
	t.Parallel()
	probeErr := errors.New("connection closed")
	stubs := &opencodeStubs{probeErr: probeErr}
	p := newTestProvisioner(t)
	reinstalled, err := p.ensureOpenCodeInstalledOn(context.Background(), "s1", stubs.probe, nil, stubs.install)
	if err == nil {
		t.Fatal("expected fail-fast error, got nil")
	}
	if !errors.Is(err, probeErr) {
		t.Errorf("error should wrap the probe error; got %v", err)
	}
	if reinstalled || stubs.installCalled {
		t.Errorf("nil statFS must not install; reinstalled=%v installCalled=%v", reinstalled, stubs.installCalled)
	}
}

// TestEnsureOpenCode_InstallFailureSurfacesOutput: an install that ran
// and failed surfaces its combined output in the error.
func TestEnsureOpenCode_InstallFailureSurfacesOutput(t *testing.T) {
	t.Parallel()
	stubs := &opencodeStubs{
		probeVersion: "0.0.1",
		installOut:   []byte("::: ERROR: bun is not on PATH"),
		installErr:   errors.New("exit status 1"),
	}
	p := newTestProvisioner(t)
	_, err := p.ensureOpenCodeInstalledOn(context.Background(), "s1", stubs.probe, newStubFS(), stubs.install)
	if err == nil {
		t.Fatal("expected install error to surface, got nil")
	}
	if !contains(err.Error(), "bun is not on PATH") {
		t.Errorf("error should carry install output; got %v", err)
	}
}

// TestIsClosedConnErr_ConnectionClosed pins the sprites-go exec.Wait
// symptom ("connection closed") as retryable — the wake-race probe
// failure that previously fell straight through to a blind install.
func TestIsClosedConnErr_ConnectionClosed(t *testing.T) {
	t.Parallel()
	if !isClosedConnErr(errors.New("connection closed")) {
		t.Error(`isClosedConnErr("connection closed") = false, want true`)
	}
}
