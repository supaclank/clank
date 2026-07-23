package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
)

// fakeCodexBin is the compiled fake codex CLI, built once in TestMain
// (see auth_anthropic_setup_token_test.go) alongside the fake claude.
var fakeCodexBin string

// buildFakeCodex compiles a helper that behaves like
// `codex login --device-auth` on codex 0.145.0: prints the sign-in
// instructions with the verification URL and one-time code each alone
// on their line, wrapped in the same ANSI SGR codes the real CLI
// emits even when piped. The behavior variant rides os.Args (the
// ceremony passes through whatever argv the login command resolver
// returns), so tests can t.Parallel() without env races:
//
//   - "happy": prints URL+code, then writes $CODEX_HOME/auth.json and
//     exits 0 — the approved-login path.
//   - "exit-clean": prints URL+code and exits 0 WITHOUT writing
//     auth.json — mimics the CLI exiting 0 on a delivered signal.
//   - "fail": prints an error and exits 1.
//   - "hang-after-print": prints URL+code then sleeps far longer than
//     any test — for CancelFlow coverage.
//   - "silent-hang": prints nothing and sleeps — for start-timeout
//     coverage.
//
// Every mode exits 9 if CODEX_API_KEY or OPENAI_API_KEY reached the
// subprocess: the ceremony must scrub inherited keys so codex can't
// short-circuit the fresh sign-in.
func buildFakeCodex() (path string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "fake-codex-")
	if err != nil {
		return "", nil, err
	}
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(fakeCodexSource), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	bin := filepath.Join(dir, "codex")
	cmd := exec.Command("go", "build", "-o", bin, src)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("go build fake codex: %w", err)
	}
	return bin, func() { _ = os.RemoveAll(dir) }, nil
}

const fakeCodexSource = `package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func main() {
	if os.Getenv("CODEX_API_KEY") != "" || os.Getenv("OPENAI_API_KEY") != "" {
		fmt.Fprintln(os.Stderr, "inherited API key reached codex login")
		os.Exit(9)
	}
	mode := "happy"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	if mode == "silent-hang" {
		time.Sleep(30 * time.Second)
		return
	}
	fmt.Println()
	fmt.Println("Welcome to Codex [v\x1b[90m0.0.0-fake\x1b[0m]")
	fmt.Println()
	fmt.Println("Follow these steps to sign in with ChatGPT using device code authorization:")
	fmt.Println()
	fmt.Println("1. Open this link in your browser and sign in to your account")
	fmt.Println("   \x1b[94mhttps://auth.example.com/codex/device\x1b[0m")
	fmt.Println()
	fmt.Println("2. Enter this one-time code \x1b[90m(expires in 15 minutes)\x1b[0m")
	fmt.Println("   \x1b[94mFAKE-C0DE1\x1b[0m")
	switch mode {
	case "happy":
		time.Sleep(200 * time.Millisecond)
		home := os.Getenv("CODEX_HOME")
		if home == "" {
			fmt.Fprintln(os.Stderr, "CODEX_HOME not set")
			os.Exit(8)
		}
		if err := os.MkdirAll(home, 0o700); err != nil {
			os.Exit(8)
		}
		if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("{\"tokens\":{\"access_token\":\"fake\"}}"), 0o600); err != nil {
			os.Exit(8)
		}
		fmt.Println("Successfully signed in with ChatGPT")
	case "exit-clean":
		// Exit 0 with no auth.json write.
	case "fail":
		fmt.Fprintln(os.Stderr, "error: device authorization is disabled for this workspace")
		os.Exit(1)
	case "hang-after-print":
		time.Sleep(30 * time.Second)
	}
}
`

// newCodexAuthManager wires a test AuthManager to the fake codex in
// the given mode. Returns the manager and its temp home dir (codex
// home resolves to <home>/.codex).
func newCodexAuthManager(t *testing.T, mode string) (*AuthManager, string) {
	t.Helper()
	a, dir := newTestAuthManager(t)
	a.codexLogin = func(context.Context) ([]string, error) {
		return []string{fakeCodexBin, mode}, nil
	}
	return a, dir
}

// awaitTerminalFlowState polls GetFlowStatus until the flow reaches a
// terminal state, then returns it.
func awaitTerminalFlowState(t *testing.T, a *AuthManager, flowID string) agent.DeviceFlowStatus {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st, err := a.GetFlowStatus(context.Background(), flowID)
		if err != nil {
			t.Fatalf("GetFlowStatus: %v", err)
		}
		switch st.State {
		case agent.DeviceFlowPending, agent.DeviceFlowAuthorized:
			time.Sleep(20 * time.Millisecond)
			continue
		default:
			return st
		}
	}
	t.Fatalf("flow %s never reached a terminal state", flowID)
	return agent.DeviceFlowStatus{}
}

func TestCodexDeviceFlow_EndToEnd(t *testing.T) {
	t.Parallel()
	a, dir := newCodexAuthManager(t, "happy")
	// A stored API key must be cleared by a successful subscription
	// connect — a lingering CODEX_API_KEY would shadow the new login.
	if err := a.SetOpenAIAPIKey("sk-old-key"); err != nil {
		t.Fatalf("seed api key: %v", err)
	}
	var callbacks atomic.Int64
	a.SetOpenAICredentialCallback(func() { callbacks.Add(1) })

	start, err := a.StartDeviceFlow(context.Background(), ProviderOpenAICodexChatGPT)
	if err != nil {
		t.Fatalf("StartDeviceFlow: %v", err)
	}
	// ANSI codes must be stripped and each value parsed off its line.
	if start.VerificationURL != "https://auth.example.com/codex/device" {
		t.Errorf("VerificationURL = %q", start.VerificationURL)
	}
	if start.UserCode != "FAKE-C0DE1" {
		t.Errorf("UserCode = %q", start.UserCode)
	}
	if start.FlowID == "" || start.ExpiresAt.IsZero() || start.Interval <= 0 {
		t.Errorf("start missing flow metadata: %+v", start)
	}

	if st := awaitTerminalFlowState(t, a, start.FlowID); st.State != agent.DeviceFlowSuccess {
		t.Fatalf("terminal state = %s (%s), want success", st.State, st.Error)
	}
	if callbacks.Load() == 0 {
		t.Error("credential callback never fired — adapters would keep stale auth")
	}
	sink, err := a.readOpenAISink()
	if err != nil {
		t.Fatalf("read sink: %v", err)
	}
	if !sink.ChatGPTConnected || sink.APIKey != "" {
		t.Errorf("sink after connect = %+v, want chatgpt_connected and no api key", sink)
	}
	if !codexCLILoginPresent(filepath.Join(dir, ".codex", "auth.json")) {
		t.Error("auth.json missing from codex home after happy flow")
	}

	infos, err := a.ListProviders(context.Background(), agent.BackendCodex)
	if err != nil {
		t.Fatalf("ListProviders: %v", err)
	}
	assertProvider(t, infos, ProviderOpenAICodexChatGPT, true, agent.CredentialSourceStore)
	// Clearing the key disconnects the API-key provider.
	assertProvider(t, infos, ProviderOpenAICodexAPI, false, "")
}

// The CLI exits 0 when killed by a signal, so exit code alone must
// never count as success — a fresh auth.json write is required.
func TestCodexDeviceFlow_CleanExitWithoutLoginFails(t *testing.T) {
	t.Parallel()
	a, _ := newCodexAuthManager(t, "exit-clean")
	start, err := a.StartDeviceFlow(context.Background(), ProviderOpenAICodexChatGPT)
	if err != nil {
		t.Fatalf("StartDeviceFlow: %v", err)
	}
	st := awaitTerminalFlowState(t, a, start.FlowID)
	if st.State != agent.DeviceFlowError || !strings.Contains(st.Error, "without completing") {
		t.Fatalf("terminal = %s (%q), want error about incomplete sign-in", st.State, st.Error)
	}
	if a.openAIChatGPTConnected() {
		t.Error("chatgpt reported connected after incomplete login")
	}
}

// Same, with a pre-existing auth.json: an old login left on disk must
// not satisfy the freshness check when the ceremony ends without a
// rewrite.
func TestCodexDeviceFlow_StaleAuthJSONDoesNotCountAsSuccess(t *testing.T) {
	t.Parallel()
	a, dir := newCodexAuthManager(t, "exit-clean")
	codexHome := filepath.Join(dir, ".codex")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(`{"tokens":{"access_token":"stale"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	start, err := a.StartDeviceFlow(context.Background(), ProviderOpenAICodexChatGPT)
	if err != nil {
		t.Fatalf("StartDeviceFlow: %v", err)
	}
	if st := awaitTerminalFlowState(t, a, start.FlowID); st.State != agent.DeviceFlowError {
		t.Fatalf("terminal = %s (%q), want error", st.State, st.Error)
	}
}

func TestCodexDeviceFlow_LoginFailureSurfacesOutput(t *testing.T) {
	t.Parallel()
	a, _ := newCodexAuthManager(t, "fail")
	start, err := a.StartDeviceFlow(context.Background(), ProviderOpenAICodexChatGPT)
	if err != nil {
		t.Fatalf("StartDeviceFlow: %v", err)
	}
	st := awaitTerminalFlowState(t, a, start.FlowID)
	if st.State != agent.DeviceFlowError {
		t.Fatalf("terminal = %s, want error", st.State)
	}
	if !strings.Contains(st.Error, "device authorization is disabled") {
		t.Errorf("error should carry the CLI's own message, got %q", st.Error)
	}
}

func TestCodexDeviceFlow_Cancel(t *testing.T) {
	t.Parallel()
	a, _ := newCodexAuthManager(t, "hang-after-print")
	start, err := a.StartDeviceFlow(context.Background(), ProviderOpenAICodexChatGPT)
	if err != nil {
		t.Fatalf("StartDeviceFlow: %v", err)
	}
	if err := a.CancelFlow(context.Background(), start.FlowID); err != nil {
		t.Fatalf("CancelFlow: %v", err)
	}
	if st := awaitTerminalFlowState(t, a, start.FlowID); st.State != agent.DeviceFlowCanceled {
		t.Fatalf("terminal = %s (%q), want canceled", st.State, st.Error)
	}
}

// A dead request context during the URL wait kills the subprocess and
// surfaces the error to the start call.
func TestCodexDeviceFlow_StartContextCanceled(t *testing.T) {
	t.Parallel()
	a, _ := newCodexAuthManager(t, "silent-hang")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := a.StartDeviceFlow(ctx, ProviderOpenAICodexChatGPT); err == nil {
		t.Fatal("StartDeviceFlow should fail when the codex CLI never prints a code")
	}
}

// The ceremony must scrub inherited OpenAI credentials — the fake
// exits 9 if either var reaches it, so success proves the scrub.
func TestCodexDeviceFlow_ScrubsInheritedKeys(t *testing.T) {
	t.Setenv(EnvCodexAPIKey, "inherited-codex-key")
	t.Setenv(EnvOpenAIAPIKey, "inherited-openai-key")
	a, _ := newCodexAuthManager(t, "happy")
	start, err := a.StartDeviceFlow(context.Background(), ProviderOpenAICodexChatGPT)
	if err != nil {
		t.Fatalf("StartDeviceFlow: %v", err)
	}
	if st := awaitTerminalFlowState(t, a, start.FlowID); st.State != agent.DeviceFlowSuccess {
		t.Fatalf("terminal = %s (%q), want success", st.State, st.Error)
	}
}

func TestStartDeviceFlow_CodexUnavailableWithoutLoginCommand(t *testing.T) {
	t.Parallel()
	a, _ := newTestAuthManager(t) // no codexLogin wired
	_, err := a.StartDeviceFlow(context.Background(), ProviderOpenAICodexChatGPT)
	if !errors.Is(err, ErrCodexDeviceAuthUnavailable) {
		t.Fatalf("err = %v, want ErrCodexDeviceAuthUnavailable", err)
	}
}

// assertProvider finds providerID in infos and checks its state.
func assertProvider(t *testing.T, infos []agent.ProviderAuthInfo, providerID string, connected bool, source string) {
	t.Helper()
	for _, p := range infos {
		if p.ProviderID != providerID {
			continue
		}
		if p.Connected != connected || p.Source != source {
			t.Errorf("%s: connected=%v source=%q, want connected=%v source=%q",
				providerID, p.Connected, p.Source, connected, source)
		}
		return
	}
	t.Errorf("provider %s not in list", providerID)
}

// The subscription reports connected only while BOTH the ceremony
// record and codex's auth.json exist: `codex logout` outside clank
// (file gone) must win over a stale sink flag.
func TestListProviders_CodexChatGPTStates(t *testing.T) {
	t.Parallel()
	a, dir := newTestAuthManager(t)
	authJSON := filepath.Join(dir, ".codex", "auth.json")

	infos, err := a.ListProviders(context.Background(), agent.BackendCodex)
	if err != nil {
		t.Fatal(err)
	}
	assertProvider(t, infos, ProviderOpenAICodexChatGPT, false, "")

	// Sink flag without auth.json (logged out behind clank's back).
	if err := a.setOpenAIChatGPTConnected(); err != nil {
		t.Fatal(err)
	}
	infos, _ = a.ListProviders(context.Background(), agent.BackendCodex)
	assertProvider(t, infos, ProviderOpenAICodexChatGPT, false, "")

	// Flag + file → connected via clank's store.
	if err := os.MkdirAll(filepath.Dir(authJSON), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authJSON, []byte(`{"tokens":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	infos, _ = a.ListProviders(context.Background(), agent.BackendCodex)
	assertProvider(t, infos, ProviderOpenAICodexChatGPT, true, agent.CredentialSourceStore)
}

// Without the ceremony record, a machine's own codex login surfaces
// only when the laptop fallback is enabled — mirroring the claude CLI
// fallback's deployment gating.
func TestListProviders_CodexCLIFallback(t *testing.T) {
	t.Parallel()
	a, dir := newTestAuthManager(t)
	authJSON := filepath.Join(dir, ".codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(authJSON), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authJSON, []byte(`{"tokens":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	infos, err := a.ListProviders(context.Background(), agent.BackendCodex)
	if err != nil {
		t.Fatal(err)
	}
	assertProvider(t, infos, ProviderOpenAICodexChatGPT, false, "")

	a.EnableCodexCLIFallback()
	infos, _ = a.ListProviders(context.Background(), agent.BackendCodex)
	assertProvider(t, infos, ProviderOpenAICodexChatGPT, true, agent.CredentialSourceCodexCLI)
}

// CODEX_API_KEY in the host environment authenticates adapter spawns
// with no clank connection; status must say so.
func TestListProviders_CodexAPIKeyEnvDetection(t *testing.T) {
	t.Parallel()
	a, _ := newTestAuthManager(t)
	a.lookupEnv = mapEnv(map[string]string{EnvCodexAPIKey: "env-key"})
	infos, err := a.ListProviders(context.Background(), agent.BackendCodex)
	if err != nil {
		t.Fatal(err)
	}
	assertProvider(t, infos, ProviderOpenAICodexAPI, true, agent.CredentialSourceEnv)
}

// Regression: DeleteCredential for the codex API key used to fall
// through to the opencode sink — clearing nothing and restarting the
// OpenCode server for no reason.
func TestDeleteCredential_CodexAPIKey(t *testing.T) {
	t.Parallel()
	a, _ := newTestAuthManager(t)
	var restarts atomic.Int64
	a.restart = func(context.Context) error { restarts.Add(1); return nil }
	var callbacks atomic.Int64
	a.SetOpenAICredentialCallback(func() { callbacks.Add(1) })
	if err := a.SetOpenAIAPIKey("sk-key"); err != nil {
		t.Fatal(err)
	}

	if err := a.DeleteCredential(context.Background(), ProviderOpenAICodexAPI); err != nil {
		t.Fatalf("DeleteCredential: %v", err)
	}
	if a.OpenAIEnv() != nil {
		t.Error("api key still resolves after delete")
	}
	if restarts.Load() != 0 {
		t.Error("codex credential delete restarted the OpenCode server")
	}
	if callbacks.Load() == 0 {
		t.Error("credential callback should fire so adapters drop the key")
	}
}

// Disconnecting the subscription deletes codex's auth.json (a real
// logout — that's the only way to stop the adapter from using it) and
// clears the ceremony record.
func TestDeleteCredential_CodexChatGPT(t *testing.T) {
	t.Parallel()
	a, dir := newTestAuthManager(t)
	var callbacks atomic.Int64
	a.SetOpenAICredentialCallback(func() { callbacks.Add(1) })
	authJSON := filepath.Join(dir, ".codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(authJSON), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authJSON, []byte(`{"tokens":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.setOpenAIChatGPTConnected(); err != nil {
		t.Fatal(err)
	}

	if err := a.DeleteCredential(context.Background(), ProviderOpenAICodexChatGPT); err != nil {
		t.Fatalf("DeleteCredential: %v", err)
	}
	if _, err := os.Stat(authJSON); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("auth.json should be removed, stat err = %v", err)
	}
	if a.openAIChatGPTConnected() {
		t.Error("chatgpt still reported connected after disconnect")
	}
	if callbacks.Load() == 0 {
		t.Error("credential callback should fire so adapters drop the login")
	}
}

// Storing an API key must not erase the record of a ChatGPT
// connection: both can be connected, codex's own resolution decides
// which credential a session uses.
func TestSetOpenAIAPIKey_PreservesChatGPTConnection(t *testing.T) {
	t.Parallel()
	a, _ := newTestAuthManager(t)
	if err := a.setOpenAIChatGPTConnected(); err != nil {
		t.Fatal(err)
	}
	if err := a.SetOpenAIAPIKey("sk-key"); err != nil {
		t.Fatal(err)
	}
	sink, err := a.readOpenAISink()
	if err != nil {
		t.Fatal(err)
	}
	if !sink.ChatGPTConnected || sink.APIKey != "sk-key" {
		t.Errorf("sink = %+v, want both credentials recorded", sink)
	}
}
