package host_test

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/supaclank/clank/internal/agent"
	acpx "github.com/supaclank/clank/internal/agent/acp"
	"github.com/supaclank/clank/internal/agent/acp/acptest"
	"github.com/supaclank/clank/internal/host"
	sdk "github.com/coder/acp-go-sdk"
)

// The codex manager exposes its login ceremony (device auth needs the
// pinned codex CLI); a generic profile manager exposes none.
func TestACPBackendManager_LoginArgvOnlyForCodex(t *testing.T) {
	t.Parallel()
	codexMgr, err := host.NewCodexACPManager(host.ACPDirs{Tools: t.TempDir()})
	if err != nil {
		t.Fatalf("NewCodexACPManager: %v", err)
	}
	if _, ok := codexMgr.LoginArgv(); !ok {
		t.Error("codex manager should expose a login command")
	}

	generic, err := host.NewACPBackendManager(acpx.AdapterProfile{
		ID:      "test-adapter",
		Backend: agent.BackendOpenCode,
		Scope:   acpx.ScopeHost,
		Command: func(string) (string, []string) { return "unused", nil },
	})
	if err != nil {
		t.Fatalf("NewACPBackendManager: %v", err)
	}
	if _, ok := generic.LoginArgv(); ok {
		t.Error("generic manager should not expose a login command")
	}
}

// End-to-end through the manager: supervisor spawn (scripted agent over
// real pipes), fresh CreateBackend → Open → Send roundtrip, and
// discovery mapping incl. the ghost-row filter.
func TestACPBackendManager_CreateOpenSendAndDiscover(t *testing.T) {
	// installGuidanceSkills writes under $HOME — isolate it.
	t.Setenv("HOME", t.TempDir())

	mgr, err := host.NewCodexACPManager(acpDirs(t))
	if err != nil {
		t.Fatalf("NewCodexACPManager: %v", err)
	}
	scripted := func(string) *acptest.ScriptedAgent {
		a := &acptest.ScriptedAgent{}
		a.PromptFn = func(ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
			_ = a.Conn().SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId, Update: sdk.UpdateAgentMessageText("ok")})
			return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
		}
		a.ListSessionsFn = func(ctx context.Context, p sdk.ListSessionsRequest) (sdk.ListSessionsResponse, error) {
			return sdk.ListSessionsResponse{Sessions: []sdk.SessionInfo{
				{SessionId: "thr-1", Cwd: "/proj/a", Title: sdk.Ptr("Fix the bug"), UpdatedAt: sdk.Ptr(time.Now().Format(time.RFC3339Nano))},
				{SessionId: "thr-ghost", Cwd: "/proj/a"}, // no title, no timestamp → filtered
			}}, nil
		}
		return a
	}
	mgr.Supervisor().SetSpawnFunc(acptest.SpawnFunc(scripted, acpx.CodexProfile("unused-bun", "unused-entry", nil), t.Logf))
	mgr.Supervisor().SetReconcileInterval(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Init(ctx, func() ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer mgr.Shutdown()

	b, err := mgr.CreateBackend(ctx, agent.BackendInvocation{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("CreateBackend: %v", err)
	}
	defer func() { _ = b.Stop() }()
	callCtx, callCancel := context.WithTimeout(ctx, 10*time.Second)
	defer callCancel()
	if err := b.OpenAndSend(callCtx, agent.SendMessageOpts{Text: "hi"}); err != nil {
		t.Fatalf("OpenAndSend: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for b.Status() != agent.StatusIdle && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	msgs, err := b.Messages(callCtx)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != 2 || msgs[1].Content != "ok" {
		t.Fatalf("Messages = %+v, want user+assistant with 'ok'", msgs)
	}

	snaps, err := mgr.DiscoverAllSessions(callCtx)
	if err != nil {
		t.Fatalf("DiscoverAllSessions: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("snapshots = %d, want 1 (ghost row filtered)", len(snaps))
	}
	s := snaps[0]
	if s.ID != "thr-1" || s.Backend != agent.BackendCodex || s.Title != "Fix the bug" || s.Directory != "/proj/a" {
		t.Errorf("snapshot = %+v", s)
	}
}

// The manager MUST implement agent.ConfigOptionsLister: Service.ConfigOptions
// type-asserts it and silently returns nil otherwise, which would ship an
// empty knob editor for every ACP backend.
func TestACPBackendManager_ImplementsConfigOptionsLister(t *testing.T) {
	t.Parallel()
	mgr, err := host.NewCodexACPManager(acpDirs(t))
	if err != nil {
		t.Fatalf("NewCodexACPManager: %v", err)
	}
	if _, ok := any(mgr).(agent.ConfigOptionsLister); !ok {
		t.Fatal("ACPBackendManager does not implement agent.ConfigOptionsLister — /config-options will return nil for every ACP backend")
	}
}

// optionsAgent scripts a session/new response advertising a model and a
// mode config option, counting sessions opened.
func optionsAgent(sessions *atomic.Int64) func(string) *acptest.ScriptedAgent {
	category := sdk.SessionConfigOptionCategory("model")
	modeCategory := sdk.SessionConfigOptionCategory("mode")
	return func(string) *acptest.ScriptedAgent {
		a := &acptest.ScriptedAgent{}
		a.NewSessionFn = func(ctx context.Context, p sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
			n := sessions.Add(1)
			return sdk.NewSessionResponse{
				SessionId: sdk.SessionId("s-" + strconv.FormatInt(n, 10)),
				ConfigOptions: []sdk.SessionConfigOption{
					{Select: &sdk.SessionConfigOptionSelect{
						Id: "model", Name: "Model", Category: &category, CurrentValue: "gpt-5.2-codex",
						Options: sdk.SessionConfigSelectOptions{Ungrouped: &sdk.SessionConfigSelectOptionsUngrouped{
							{Value: "gpt-5.2-codex", Name: "GPT-5.2 Codex"},
						}},
					}},
					{Select: &sdk.SessionConfigOptionSelect{
						Id: "mode", Name: "Mode", Category: &modeCategory, CurrentValue: "agent",
						Options: sdk.SessionConfigSelectOptions{Ungrouped: &sdk.SessionConfigSelectOptionsUngrouped{
							{Value: "read-only", Name: "Read Only"},
							{Value: "agent", Name: "Agent"},
						}},
					}},
				},
			}, nil
		}
		return a
	}
}

// ConfigOptions opens one short-lived session and returns what the agent
// advertises, verbatim. It is deliberately uncached: a second call probes
// again, so the editor always shows live truth (stale-cache staleness was
// the prewarm design's failure mode).
func TestACPBackendManager_ConfigOptionsProbesOnDemand(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // installGuidanceSkills writes under $HOME

	mgr, err := host.NewCodexACPManager(acpDirs(t))
	if err != nil {
		t.Fatalf("NewCodexACPManager: %v", err)
	}
	var sessions atomic.Int64
	mgr.Supervisor().SetSpawnFunc(acptest.SpawnFunc(optionsAgent(&sessions), acpx.CodexProfile("bun", "entry", nil), t.Logf))
	mgr.Supervisor().SetReconcileInterval(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Init(ctx, func() ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer mgr.Shutdown()

	opts, err := mgr.ConfigOptions(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("ConfigOptions: %v", err)
	}
	if len(opts) != 2 {
		t.Fatalf("ConfigOptions = %+v, want the 2 advertised options", opts)
	}
	if opts[0].ID != "model" || opts[0].CurrentValue != "gpt-5.2-codex" || len(opts[0].Values) != 1 {
		t.Errorf("model option = %+v, want the advertised values verbatim", opts[0])
	}
	if opts[1].ID != "mode" || len(opts[1].Values) != 2 || opts[1].Values[0].Value != "read-only" {
		t.Errorf("mode option = %+v, want the advertised values verbatim", opts[1])
	}
	if got := sessions.Load(); got != 1 {
		t.Fatalf("probe opened %d sessions, want 1", got)
	}

	// No cache: the next editor open probes again.
	if _, err := mgr.ConfigOptions(ctx, t.TempDir()); err != nil {
		t.Fatalf("second ConfigOptions: %v", err)
	}
	if got := sessions.Load(); got != 2 {
		t.Fatalf("second call reused a cache (%d sessions total), want a fresh probe (2)", got)
	}
}

// Concurrent callers share one probe. A host-scoped backend advertises the
// same options everywhere, so even different dirs coalesce onto one
// in-flight session.
func TestACPBackendManager_ConfigOptionsSingleFlightsConcurrentCallers(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // installGuidanceSkills writes under $HOME

	mgr, err := host.NewCodexACPManager(acpDirs(t))
	if err != nil {
		t.Fatalf("NewCodexACPManager: %v", err)
	}
	category := sdk.SessionConfigOptionCategory("model")
	var sessions atomic.Int64
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	scripted := func(string) *acptest.ScriptedAgent {
		a := &acptest.ScriptedAgent{}
		a.NewSessionFn = func(ctx context.Context, p sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
			n := sessions.Add(1)
			startedOnce.Do(func() { close(started) })
			<-release
			return sdk.NewSessionResponse{
				SessionId: sdk.SessionId("s-" + strconv.FormatInt(n, 10)),
				ConfigOptions: []sdk.SessionConfigOption{{Select: &sdk.SessionConfigOptionSelect{
					Id: "model", Name: "Model", Category: &category, CurrentValue: "gpt-5.2-codex",
					Options: sdk.SessionConfigSelectOptions{Ungrouped: &sdk.SessionConfigSelectOptionsUngrouped{
						{Value: "gpt-5.2-codex", Name: "GPT-5.2 Codex"},
					}},
				}}},
			}, nil
		}
		return a
	}
	mgr.Supervisor().SetSpawnFunc(acptest.SpawnFunc(scripted, acpx.CodexProfile("bun", "entry", nil), t.Logf))
	mgr.Supervisor().SetReconcileInterval(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Init(ctx, func() ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer mgr.Shutdown()

	results := make(chan int, 2)
	errs := make(chan error, 2)
	for _, dir := range []string{t.TempDir(), t.TempDir()} {
		go func(d string) {
			opts, err := mgr.ConfigOptions(ctx, d)
			errs <- err
			results <- len(opts)
		}(dir)
	}
	<-started      // one probe is in flight; the second caller must wait on it
	close(release) // let it finish
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("ConfigOptions: %v", err)
		}
		if n := <-results; n != 1 {
			t.Errorf("caller got %d options, want 1", n)
		}
	}
	if got := sessions.Load(); got != 1 {
		t.Fatalf("concurrent callers opened %d sessions, want 1 (single-flighted)", got)
	}
}

// A per-dir backend (opencode) advertises per-repo state, so different
// dirs must NOT coalesce — each gets its own probe.
func TestACPBackendManager_ConfigOptionsKeysPerDirForPerDirScope(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // installGuidanceSkills writes under $HOME

	mgr := newPerDirManager(t)
	var sessions atomic.Int64
	mgr.Supervisor().SetSpawnFunc(acptest.SpawnFunc(optionsAgent(&sessions), acpx.CodexProfile("bun", "entry", nil), t.Logf))
	mgr.Supervisor().SetReconcileInterval(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Init(ctx, func() ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer mgr.Shutdown()

	if _, err := mgr.ConfigOptions(ctx, t.TempDir()); err != nil {
		t.Fatalf("ConfigOptions dir A: %v", err)
	}
	if _, err := mgr.ConfigOptions(ctx, t.TempDir()); err != nil {
		t.Fatalf("ConfigOptions dir B: %v", err)
	}
	if got := sessions.Load(); got != 2 {
		t.Fatalf("two dirs opened %d sessions, want 2 (per-dir options must not coalesce)", got)
	}
}

// A failed probe surfaces its error — never a silent empty list the
// editor would render as "this agent has no knobs" — and the next call
// retries instead of being stuck behind a poisoned cache entry.
func TestACPBackendManager_ConfigOptionsFailureSurfacesAndRetries(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // installGuidanceSkills writes under $HOME

	mgr, err := host.NewCodexACPManager(acpDirs(t))
	if err != nil {
		t.Fatalf("NewCodexACPManager: %v", err)
	}
	category := sdk.SessionConfigOptionCategory("model")
	var attempts atomic.Int64
	scripted := func(string) *acptest.ScriptedAgent {
		a := &acptest.ScriptedAgent{}
		a.NewSessionFn = func(ctx context.Context, p sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
			if attempts.Add(1) == 1 {
				return sdk.NewSessionResponse{}, errors.New("adapter not ready yet")
			}
			return sdk.NewSessionResponse{
				SessionId: "s-2",
				ConfigOptions: []sdk.SessionConfigOption{{Select: &sdk.SessionConfigOptionSelect{
					Id: "model", Name: "Model", Category: &category, CurrentValue: "gpt-5.2-codex",
					Options: sdk.SessionConfigSelectOptions{Ungrouped: &sdk.SessionConfigSelectOptionsUngrouped{
						{Value: "gpt-5.2-codex", Name: "GPT-5.2 Codex"},
					}},
				}}},
			}, nil
		}
		return a
	}
	mgr.Supervisor().SetSpawnFunc(acptest.SpawnFunc(scripted, acpx.CodexProfile("bun", "entry", nil), t.Logf))
	mgr.Supervisor().SetReconcileInterval(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Init(ctx, func() ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer mgr.Shutdown()

	if _, err := mgr.ConfigOptions(ctx, t.TempDir()); err == nil {
		t.Fatal("failed probe returned nil error — the editor would render a bogus empty knob list")
	}
	opts, err := mgr.ConfigOptions(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("retry after failed probe: %v", err)
	}
	if len(opts) != 1 {
		t.Fatalf("retry options = %+v, want the advertised list", opts)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("probe attempts = %d, want 2 (one failure + one retry)", got)
	}
}

// After Shutdown no new probes may start — callers get an error, not a
// hang on a supervisor that will never spawn again.
func TestACPBackendManager_ConfigOptionsAfterShutdownFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // installGuidanceSkills writes under $HOME

	mgr, err := host.NewCodexACPManager(acpDirs(t))
	if err != nil {
		t.Fatalf("NewCodexACPManager: %v", err)
	}
	var sessions atomic.Int64
	mgr.Supervisor().SetSpawnFunc(acptest.SpawnFunc(optionsAgent(&sessions), acpx.CodexProfile("bun", "entry", nil), t.Logf))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Init(ctx, func() ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("Init: %v", err)
	}
	mgr.Shutdown()

	if _, err := mgr.ConfigOptions(ctx, t.TempDir()); err == nil {
		t.Fatal("ConfigOptions after Shutdown must fail fast, not probe a dead supervisor")
	}
}

// acpDirs gives a manager an isolated throwaway tools dir so a test never
// touches the developer's real ~/.clank state.
func acpDirs(t *testing.T) host.ACPDirs {
	t.Helper()
	return host.ACPDirs{Tools: t.TempDir()}
}

// newPerDirManager builds a manager with per-dir scope (opencode's shape),
// so config-option probes key per dir. It reuses the codex scripted
// profile but flips the scope.
func newPerDirManager(t *testing.T) *host.ACPBackendManager {
	t.Helper()
	profile := acpx.CodexProfile("bun", "entry", nil)
	profile.Scope = acpx.ScopePerDir
	mgr, err := host.NewACPBackendManager(profile)
	if err != nil {
		t.Fatalf("NewACPBackendManager: %v", err)
	}
	return mgr
}
