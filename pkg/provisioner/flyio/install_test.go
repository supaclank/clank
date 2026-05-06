package flyio

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"log"
	"sync"
	"syscall"
	"testing"
	"time"
)

// stubFS records fs op order so tests can pin remove-before-write.
type stubFS struct {
	mu    sync.Mutex
	files map[string][]byte
	calls []string // "Remove:/p", "Write:/p", in order
	// onWrite replaces the default succeed-and-store path to simulate
	// write failures.
	onWrite func(name string, data []byte) error
}

func newStubFS() *stubFS {
	return &stubFS{files: map[string][]byte{}}
}

func (s *stubFS) Open(name string) (fs.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.files[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return &stubFile{name: name, r: bytes.NewReader(data), size: int64(len(data))}, nil
}

// stubFile is a tiny fs.File over a []byte; just enough for fs.Stat.
type stubFile struct {
	name string
	size int64
	r    *bytes.Reader
}

func (f *stubFile) Stat() (fs.FileInfo, error) {
	return stubFileInfo{name: f.name, size: f.size}, nil
}
func (f *stubFile) Read(p []byte) (int, error) { return f.r.Read(p) }
func (f *stubFile) Close() error               { return nil }

var _ io.Reader = (*stubFile)(nil)

// Stat satisfies fs.StatFS so fs.Stat hits stubFS directly.
func (s *stubFS) Stat(name string) (fs.FileInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.files[name]
	if !ok {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
	}
	return stubFileInfo{name: name, size: int64(len(data))}, nil
}

func (s *stubFS) WriteFileContext(_ context.Context, name string, data []byte, _ fs.FileMode) error {
	s.mu.Lock()
	if s.onWrite != nil {
		fn := s.onWrite
		s.mu.Unlock()
		return fn(name, data)
	}
	s.calls = append(s.calls, "Write:"+name)
	s.files[stripLeadingSlash(name)] = append([]byte(nil), data...)
	s.mu.Unlock()
	return nil
}

func (s *stubFS) RemoveContext(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "Remove:"+name)
	if _, ok := s.files[stripLeadingSlash(name)]; !ok {
		return &fs.PathError{Op: "remove", Path: name, Err: fs.ErrNotExist}
	}
	delete(s.files, stripLeadingSlash(name))
	return nil
}

func (s *stubFS) seed(name string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[stripLeadingSlash(name)] = append([]byte(nil), data...)
}

func (s *stubFS) ops() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.calls))
	copy(out, s.calls)
	return out
}

func stripLeadingSlash(p string) string {
	if len(p) > 0 && p[0] == '/' {
		return p[1:]
	}
	return p
}

type stubFileInfo struct {
	name string
	size int64
}

func (s stubFileInfo) Name() string       { return s.name }
func (s stubFileInfo) Size() int64        { return s.size }
func (s stubFileInfo) Mode() fs.FileMode  { return 0o755 }
func (s stubFileInfo) ModTime() time.Time { return time.Time{} }
func (s stubFileInfo) IsDir() bool        { return false }
func (s stubFileInfo) Sys() any           { return nil }

func newTestProvisioner(t *testing.T) *Provisioner {
	t.Helper()
	return &Provisioner{log: log.New(testWriter{t: t}, "", 0)}
}

type testWriter struct{ t *testing.T }

func (tw testWriter) Write(p []byte) (int, error) {
	tw.t.Log(string(p))
	return len(p), nil
}

// hashHex computes the helper's expected sidecar value.
func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestEnsureBinaryInstalled_SkipsOnSizeAndHashMatch pins the fast path:
// when both size AND sidecar hash match, the upload is skipped.
func TestEnsureBinaryInstalled_SkipsOnSizeAndHashMatch(t *testing.T) {
	t.Parallel()
	want := []byte("hello world binary contents")
	stub := newStubFS()
	stub.seed("usr/local/bin/clank-host", want)
	stub.seed("usr/local/bin/clank-host"+hashSidecarSuffix, []byte(hashHex(want)))

	p := newTestProvisioner(t)
	if err := p.ensureBinaryInstalledOn(context.Background(), stub, stub, "/usr/local/bin/clank-host", want, hashHex(want)); err != nil {
		t.Fatalf("install: %v", err)
	}
	if got := stub.ops(); len(got) != 0 {
		t.Errorf("unexpected fs ops on hash-match fast path: %v", got)
	}
}

// TestEnsureBinaryInstalled_SameSizeDifferentHashReinstalls is the
// regression CR raised: two builds with the same byte length but
// different content should still trigger a reinstall, not a silent
// keep of the old binary.
func TestEnsureBinaryInstalled_SameSizeDifferentHashReinstalls(t *testing.T) {
	t.Parallel()
	old := []byte("0123456789abcdef")
	new := []byte("fedcba9876543210") // same length, different content
	if len(old) != len(new) {
		t.Fatal("test bug: old and new must be the same length")
	}
	stub := newStubFS()
	stub.seed("usr/local/bin/clank-host", old)
	stub.seed("usr/local/bin/clank-host"+hashSidecarSuffix, []byte(hashHex(old)))

	p := newTestProvisioner(t)
	if err := p.ensureBinaryInstalledOn(context.Background(), stub, stub, "/usr/local/bin/clank-host", new, hashHex(new)); err != nil {
		t.Fatalf("install: %v", err)
	}
	ops := stub.ops()
	hasBinaryWrite := false
	for _, op := range ops {
		if op == "Write:/usr/local/bin/clank-host" {
			hasBinaryWrite = true
		}
	}
	if !hasBinaryWrite {
		t.Errorf("size matched but hash differed; expected reinstall (Write:%s) in ops=%v", "/usr/local/bin/clank-host", ops)
	}
	// Sidecar should have been refreshed with the new hash.
	stub.mu.Lock()
	got := string(stub.files["usr/local/bin/clank-host"+hashSidecarSuffix])
	stub.mu.Unlock()
	if got != hashHex(new) {
		t.Errorf("sidecar hash after reinstall: got %q, want %q", got, hashHex(new))
	}
}

// TestEnsureBinaryInstalled_ColdInstallWritesBinaryAndSidecar: a fresh
// sprite (no prior file) goes Remove → Write binary → Write sidecar.
func TestEnsureBinaryInstalled_ColdInstallWritesBinaryAndSidecar(t *testing.T) {
	t.Parallel()
	stub := newStubFS()
	want := []byte("fresh binary")

	p := newTestProvisioner(t)
	if err := p.ensureBinaryInstalledOn(context.Background(), stub, stub, "/usr/local/bin/clank-host", want, hashHex(want)); err != nil {
		t.Fatalf("install: %v", err)
	}
	got := stub.ops()
	if len(got) != 3 ||
		got[0] != "Remove:/usr/local/bin/clank-host" ||
		got[1] != "Write:/usr/local/bin/clank-host" ||
		got[2] != "Write:/usr/local/bin/clank-host"+hashSidecarSuffix {
		t.Errorf("ops = %v, want [Remove:bin, Write:bin, Write:sidecar]", got)
	}
}

// TestEnsureBinaryInstalled_RemoveBeforeWrite is the ETXTBSY
// regression: stale-binary replacement must Remove before Write or
// Linux rejects the write to a running executable.
func TestEnsureBinaryInstalled_RemoveBeforeWrite(t *testing.T) {
	t.Parallel()
	stub := newStubFS()
	stub.seed("usr/local/bin/clank-host", []byte("old binary, smaller"))
	want := []byte("new binary, larger payload")

	p := newTestProvisioner(t)
	if err := p.ensureBinaryInstalledOn(context.Background(), stub, stub, "/usr/local/bin/clank-host", want, hashHex(want)); err != nil {
		t.Fatalf("install: %v", err)
	}
	got := stub.ops()
	if len(got) < 2 {
		t.Fatalf("expected at least 2 ops, got %v", got)
	}
	if got[0] != "Remove:/usr/local/bin/clank-host" {
		t.Errorf("first op should be Remove (avoid ETXTBSY); got %q", got[0])
	}
	if got[1] != "Write:/usr/local/bin/clank-host" {
		t.Errorf("second op should be Write; got %q", got[1])
	}
}

// TestEnsureBinaryInstalled_RemoveErrorIsBestEffort: a non-ENOENT
// Remove failure is tolerated and Write proceeds.
func TestEnsureBinaryInstalled_RemoveErrorIsBestEffort(t *testing.T) {
	t.Parallel()
	stub := newStubFS()
	stub.seed("usr/local/bin/clank-host", []byte("old"))
	want := []byte("new binary")

	calls := []string{}
	wfStub := stubWF{
		write: stub.WriteFileContext,
		remove: func(_ context.Context, name string) error {
			calls = append(calls, "Remove:"+name)
			return &fs.PathError{Op: "remove", Path: name, Err: syscall.EACCES}
		},
	}

	p := newTestProvisioner(t)
	if err := p.ensureBinaryInstalledOn(context.Background(), stub, wfStub, "/usr/local/bin/clank-host", want, hashHex(want)); err != nil {
		t.Fatalf("install: %v (Write should have proceeded despite Remove failure)", err)
	}
	if len(calls) != 1 || calls[0] != "Remove:/usr/local/bin/clank-host" {
		t.Errorf("Remove not called once: %v", calls)
	}
	got := stub.ops()
	hasWrite := false
	for _, op := range got {
		if op == "Write:/usr/local/bin/clank-host" {
			hasWrite = true
			break
		}
	}
	if !hasWrite {
		t.Errorf("Write should have run after Remove failure; ops = %v", got)
	}
}

// TestEnsureBinaryInstalled_WriteErrorPropagates: a write failure on
// the binary itself is surfaced, not swallowed.
func TestEnsureBinaryInstalled_WriteErrorPropagates(t *testing.T) {
	t.Parallel()
	stub := newStubFS()
	bang := errors.New("simulated write failure")
	stub.onWrite = func(_ string, _ []byte) error { return bang }

	p := newTestProvisioner(t)
	err := p.ensureBinaryInstalledOn(context.Background(), stub, stub, "/usr/local/bin/clank-host", []byte("data"), hashHex([]byte("data")))
	if err == nil {
		t.Fatal("expected write error to surface, got nil")
	}
	if !errors.Is(err, bang) && !contains(err.Error(), bang.Error()) {
		t.Errorf("error should wrap or contain %q, got %v", bang, err)
	}
}

// stubWF lets tests override Remove independently of the read path.
type stubWF struct {
	write  func(ctx context.Context, name string, data []byte, perm fs.FileMode) error
	remove func(ctx context.Context, name string) error
}

func (s stubWF) WriteFileContext(ctx context.Context, name string, data []byte, perm fs.FileMode) error {
	return s.write(ctx, name, data, perm)
}
func (s stubWF) RemoveContext(ctx context.Context, name string) error { return s.remove(ctx, name) }

// contains keeps "strings" out of this test file's import list.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
