package clankcli

import (
	"bytes"
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/config"
	daemonclient "github.com/acksell/clank/internal/daemonclient"
	"github.com/acksell/clank/internal/host"
	"github.com/acksell/clank/internal/host/hosttest"
	hostmux "github.com/acksell/clank/internal/host/mux"
	hoststore "github.com/acksell/clank/internal/host/store"
)

// newTestHost mounts a real host service (real store, real mux, stub
// backend) behind an HTTP test server and returns a daemonclient wired
// to it. Only the backend process boundary is stubbed.
func newTestHost(t *testing.T) (*daemonclient.Client, *hosttest.StubBackendManager) {
	t.Helper()

	stub := &hosttest.StubBackendManager{}
	hs, err := hoststore.Open(filepath.Join(t.TempDir(), "host.db"))
	if err != nil {
		t.Fatalf("hoststore.Open: %v", err)
	}
	t.Cleanup(func() { hs.Close() })

	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: stub,
		},
		SessionsStore: hs,
	})
	t.Cleanup(svc.Shutdown)

	srv := httptest.NewServer(hostmux.New(svc, nil).Handler())
	t.Cleanup(srv.Close)

	return daemonclient.NewTCPClient(srv.URL, ""), stub
}

// TestRunPlease_CreatesSessionAndRecordsLastSession pins the headless
// contract: one Create call carries the prompt (the host dispatches it
// via OpenAndSend — no TUI, no SSE), and the session is recorded as the
// cwd's last session so a bare `clank` reopens it.
func TestRunPlease_CreatesSessionAndRecordsLastSession(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir())

	client, stub := newTestHost(t)
	repo := hosttest.InitGitRepo(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var out bytes.Buffer
	err := runPlease(ctx, client, &out, pleaseOpts{
		backend:    agent.BackendOpenCode,
		projectDir: repo,
		prompt:     "install a release build to my phone",
	})
	if err != nil {
		t.Fatalf("runPlease: %v", err)
	}

	last := stub.Last()
	if last == nil {
		t.Fatal("no backend created — session was not started")
	}
	if !last.OpenAndSendCalled() {
		t.Error("initial prompt was not dispatched via OpenAndSend")
	}
	if got := last.LastSendOpts().Text; got != "install a release build to my phone" {
		t.Errorf("prompt received by backend: got %q", got)
	}

	prefs, err := config.LoadPreferences()
	if err != nil {
		t.Fatalf("LoadPreferences: %v", err)
	}
	sessionID := prefs.LastSessionByCwd[repo]
	if sessionID == "" {
		t.Fatalf("LastSessionByCwd[%q] not recorded; got %+v", repo, prefs.LastSessionByCwd)
	}
	if !strings.Contains(out.String(), sessionID) {
		t.Errorf("confirmation output %q does not mention session id %q", out.String(), sessionID)
	}
	if !strings.Contains(out.String(), "run 'clank' to open it") {
		t.Errorf("confirmation output %q lacks the reopen hint", out.String())
	}
}

// TestPleaseCmd_RequiresPrompt: no args and nothing piped must fail
// fast without touching the daemon.
func TestPleaseCmd_RequiresPrompt(t *testing.T) {
	t.Parallel()

	cmd := pleaseCmd()
	cmd.SetArgs([]string{})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected an arg-count error for a missing prompt, got nil")
	}
}

// TestPleaseCmd_AliasesRoute pins that `pls` and `p` reach the please
// command from the root, so shell muscle memory can rely on them.
func TestPleaseCmd_AliasesRoute(t *testing.T) {
	t.Parallel()

	for _, alias := range []string{"please", "pls", "p"} {
		cmd, _, err := Command().Find([]string{alias, "hello"})
		if err != nil {
			t.Fatalf("Find(%q): %v", alias, err)
		}
		if cmd.Name() != "please" {
			t.Errorf("Find(%q) routed to %q, want please", alias, cmd.Name())
		}
	}
}

// TestPreviewPrompt_Truncates keeps the confirmation line one-line.
func TestPreviewPrompt_Truncates(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", promptPreviewMaxLen+10)
	got := previewPrompt(long)
	if len([]rune(got)) != promptPreviewMaxLen+1 { // +1 for the ellipsis
		t.Errorf("truncated length: got %d runes", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated preview %q lacks ellipsis", got)
	}
	if short := previewPrompt("short"); short != "short" {
		t.Errorf("short prompt altered: %q", short)
	}
}
