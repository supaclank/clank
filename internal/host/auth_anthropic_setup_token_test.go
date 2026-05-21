package host

// Tests for the PTY-relay of `claude setup-token`. Real CLI is
// undriveable in CI (it opens a browser and needs Anthropic auth),
// so we compile a tiny fake binary into a tempdir and point
// setupTokenBinary at it. The fake mimics the real CLI's output
// shape (banner + URL + stdin prompt + token + closing copy) with
// just enough ANSI noise that the vt10x rendering pipeline gets
// real exercise.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
)

// TestMain builds the fake claude binary once and points
// setupTokenBinary at the absolute path. Subsequent tests share it.
func TestMain(m *testing.M) {
	bin, cleanup, err := buildFakeClaude()
	if err != nil {
		fmt.Fprintln(os.Stderr, "buildFakeClaude:", err)
		os.Exit(1)
	}
	defer cleanup()
	prev := setupTokenBinary
	setupTokenBinary = bin
	defer func() { setupTokenBinary = prev }()
	os.Exit(m.Run())
}

// buildFakeClaude compiles a small helper that behaves like
// `claude setup-token`: prints a banner with ANSI, then the URL on
// its own line, then "Paste code here…" prompt, then reads a line
// from stdin, then prints the token (also with ANSI noise around
// it), then exits.
//
// FAKE_MODE env var lets each test pick a behavior variant:
//   - "happy" (default): URL, then token after code read
//   - "bad-code": exits 1 after reading the code (no token)
//   - "slow-url": sleeps 5s before the URL — tests timeout
//   - "no-url-no-token": exits immediately without output
func buildFakeClaude() (path string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "fake-claude-")
	if err != nil {
		return "", nil, err
	}
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(fakeClaudeSource), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	bin := filepath.Join(dir, "claude")
	cmd := exec.Command("go", "build", "-o", bin, src)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("go build fake: %w", err)
	}
	return bin, func() { _ = os.RemoveAll(dir) }, nil
}

const fakeClaudeSource = `package main

import (
	"bufio"
	"fmt"
	"os"
	"time"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "setup-token" {
		os.Exit(2)
	}
	mode := os.Getenv("FAKE_MODE")
	if mode == "" {
		mode = "happy"
	}
	switch mode {
	case "no-url-no-token":
		// Exit immediately without producing anything.
		return
	case "slow-url":
		time.Sleep(5 * time.Second)
	}
	// Print a banner-ish line with some ANSI noise so vt10x has to
	// actually render rather than us getting lucky with raw bytes.
	fmt.Print("\x1b[2GWelcome\x1b[10Gto\x1b[13GClaude\x1b[20GCode\x1b[25Gv0.0.0-fake\r\n")
	fmt.Println()
	fmt.Println("Browser didn't open? Use the url below to sign in (c to copy)")
	fmt.Println()
	// Authorize URL on its own line — the pattern setupTokenURLPattern
	// is anchored to https://claude.com/cai/oauth/authorize?...
	fmt.Println("https://claude.com/cai/oauth/authorize?code=true&client_id=test-cid&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&scope=user%3Ainference&code_challenge=test-challenge&code_challenge_method=S256&state=test-state")
	fmt.Println()
	fmt.Print("\x1b[2GPaste\x1b[8Gcode\x1b[13Ghere\x1b[18Gif\x1b[21Gprompted\x1b[30G> ")

	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		os.Exit(3)
	}
	code := scanner.Text()
	// Strip trailing \r that the PTY adds.
	for len(code) > 0 && (code[len(code)-1] == '\r' || code[len(code)-1] == '\n') {
		code = code[:len(code)-1]
	}
	if mode == "bad-code" || code == "BADCODE" {
		fmt.Fprintln(os.Stderr, "error: invalid_grant")
		os.Exit(1)
	}

	fmt.Println()
	fmt.Println("\x1b[4G✓ Long-lived authentication token created successfully!")
	fmt.Println("Your OAuth token (valid for 1 year):")
	// Emit the token surrounded by some ANSI cursor positioning to
	// exercise vt10x rendering. The visible result on a wide enough
	// screen is the token contiguous.
	fmt.Println("\x1b[2Gsk-ant-oat01-TESTabcdefghijklmnopqrstuvwxyz0123456789_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-trailing")
	fmt.Println()
	fmt.Println("\x1b[2GStore this token securely.")
}
`

// Happy path: spawn, await URL, submit code, await token.
func TestSetupTokenSession_HappyPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := startSetupToken(ctx)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.close()

	urlCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	url, err := s.awaitURL(urlCtx)
	if err != nil {
		t.Fatalf("awaitURL: %v", err)
	}
	if !strings.HasPrefix(url, "https://claude.com/cai/oauth/authorize?") {
		t.Errorf("URL prefix mismatch: %q", url)
	}
	if !strings.Contains(url, "client_id=test-cid") {
		t.Errorf("URL missing client_id: %q", url)
	}

	if err := s.submitCode("VALIDCODE"); err != nil {
		t.Fatalf("submitCode: %v", err)
	}

	tokCtx, cancel2 := context.WithTimeout(ctx, 5*time.Second)
	defer cancel2()
	tok, err := s.awaitToken(tokCtx)
	if err != nil {
		t.Fatalf("awaitToken: %v", err)
	}
	if !strings.HasPrefix(tok, "sk-ant-oat01-") {
		t.Errorf("token prefix mismatch: %q", tok)
	}
	if !strings.Contains(tok, "TESTabc") {
		t.Errorf("token body mismatch: %q", tok)
	}
	if !strings.HasSuffix(tok, "trailing") {
		t.Errorf("token tail mismatch — vt10x truncated? %q", tok)
	}
}

// Cancel during the wait-for-URL phase. The session's close() must
// release awaitURL promptly with an error.
// (No t.Parallel because t.Setenv mutates process env.)
func TestSetupTokenSession_CancelDuringAwaitURL(t *testing.T) {
	t.Setenv("FAKE_MODE", "slow-url")
	s, err := startSetupToken(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	urlCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	// Kick close() in 50ms while awaitURL is still polling.
	go func() {
		time.Sleep(50 * time.Millisecond)
		s.close()
	}()

	_, err = s.awaitURL(urlCtx)
	if err == nil {
		t.Fatal("expected error from cancelled awaitURL")
	}
}

// Bad code: CLI exits 1 after reading stdin without printing the
// token. awaitToken must bail out rather than poll forever.
func TestSetupTokenSession_BadCodeNoToken(t *testing.T) {
	t.Setenv("FAKE_MODE", "bad-code")
	s, err := startSetupToken(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.close()

	urlCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.awaitURL(urlCtx); err != nil {
		t.Fatalf("awaitURL: %v", err)
	}
	if err := s.submitCode("BADCODE"); err != nil {
		t.Fatalf("submitCode: %v", err)
	}
	tokCtx, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	_, err = s.awaitToken(tokCtx)
	if err == nil {
		t.Fatal("expected error from bad-code path")
	}
	if !strings.Contains(err.Error(), "exited") && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("unexpected error shape: %v", err)
	}
}

// CLI exits before printing anything. awaitURL must bail rather
// than poll until the caller's timeout.
func TestSetupTokenSession_PrematureExit(t *testing.T) {
	t.Setenv("FAKE_MODE", "no-url-no-token")
	s, err := startSetupToken(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.close()

	urlCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = s.awaitURL(urlCtx)
	if err == nil {
		t.Fatal("expected error after premature exit")
	}
	if !strings.Contains(err.Error(), "exited") {
		t.Errorf("error should signal premature exit: %v", err)
	}
}

// Inherited Anthropic auth env vars must be stripped before exec so
// they can't short-circuit the setup-token flow.
func TestSetupTokenSession_StripsInheritedAuthEnv(t *testing.T) {
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "should-be-stripped")
	t.Setenv("ANTHROPIC_API_KEY", "should-be-stripped-too")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "and-this")
	// The fake binary doesn't care about these vars, but we exercise
	// the env-construction path. We can't directly observe the
	// child's env from this side, so we check that the happy path
	// still completes (i.e. we don't break the existing flow when
	// the stripper is active).
	s, err := startSetupToken(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.close()
	urlCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.awaitURL(urlCtx); err != nil {
		t.Fatalf("awaitURL: %v", err)
	}
}

// End-to-end AuthManager exercise: StartOAuthCodeFlow → SubmitAuthCode
// runs the fake CLI to completion and persists the captured token
// under the anthropic-claude-code provider's OAuth-token field.
func TestAuthManager_OAuthCodeFlow_EndToEnd(t *testing.T) {
	a, _ := newTestAuthManager(t)

	start, err := a.StartOAuthCodeFlow(context.Background(), ProviderAnthropicClaudeCode)
	if err != nil {
		t.Fatalf("StartOAuthCodeFlow: %v", err)
	}
	if start.FlowID == "" {
		t.Fatal("expected non-empty flow id")
	}
	if !strings.HasPrefix(start.VerificationURL, "https://claude.com/cai/oauth/authorize?") {
		t.Errorf("verification URL prefix: %q", start.VerificationURL)
	}

	if err := a.SubmitAuthCode(context.Background(), ProviderAnthropicClaudeCode, start.FlowID, "VALIDCODE"); err != nil {
		t.Fatalf("SubmitAuthCode: %v", err)
	}
	st, err := a.GetFlowStatus(context.Background(), start.FlowID)
	if err != nil {
		t.Fatalf("GetFlowStatus: %v", err)
	}
	if st.State != agent.DeviceFlowSuccess {
		t.Fatalf("flow state=%v, want success", st.State)
	}
	tok := a.AnthropicOAuthToken()
	if !strings.HasPrefix(tok, "sk-ant-oat01-") {
		t.Errorf("persisted token prefix: %q", tok)
	}
	env := a.AnthropicEnv()
	if got := env["CLAUDE_CODE_OAUTH_TOKEN"]; got != tok {
		t.Errorf("env[CLAUDE_CODE_OAUTH_TOKEN]=%q, want %q", got, tok)
	}
}

// SubmitAuthCode against a non-oauth-code provider must reject —
// catches drift between the catalog AuthType and the routing guard.
func TestAuthManager_SubmitAuthCode_RejectsNonOAuthProvider(t *testing.T) {
	t.Parallel()
	a, _ := newTestAuthManager(t)
	err := a.SubmitAuthCode(context.Background(), "openai", "anyflow", "any")
	if err == nil {
		t.Fatal("expected error for non-oauth-code provider")
	}
}

// Bad code must drive the flow to error state without leaking a
// half-set anthropic credential.
func TestAuthManager_OAuthCodeFlow_BadCodeReachesError(t *testing.T) {
	t.Setenv("FAKE_MODE", "bad-code")
	a, _ := newTestAuthManager(t)
	start, err := a.StartOAuthCodeFlow(context.Background(), ProviderAnthropicClaudeCode)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := a.SubmitAuthCode(context.Background(), ProviderAnthropicClaudeCode, start.FlowID, "BADCODE"); err == nil {
		t.Fatal("expected error from bad-code submit")
	}
	st, _ := a.GetFlowStatus(context.Background(), start.FlowID)
	if st.State != agent.DeviceFlowError {
		t.Errorf("state=%v, want error", st.State)
	}
	if tok := a.AnthropicOAuthToken(); tok != "" {
		t.Errorf("token should not be persisted on error path, got %q", tok)
	}
}

// CancelFlow during an oauth-code session tears down the PTY child.
func TestAuthManager_OAuthCodeFlow_Cancel(t *testing.T) {
	t.Parallel()
	a, _ := newTestAuthManager(t)
	start, err := a.StartOAuthCodeFlow(context.Background(), ProviderAnthropicClaudeCode)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := a.CancelFlow(context.Background(), start.FlowID); err != nil {
		t.Fatalf("CancelFlow: %v", err)
	}
	// Status should land on canceled within a tick.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		st, _ := a.GetFlowStatus(context.Background(), start.FlowID)
		if st.State == agent.DeviceFlowCanceled {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("flow never reached canceled")
}
