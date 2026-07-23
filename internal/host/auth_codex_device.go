package host

// ChatGPT-subscription auth for the codex backend via the codex CLI's
// own device-code login. clank spawns the pinned codex headless
// (`codex login --device-auth` prints a verification URL and one-time
// code even with no TTY), relays URL + code to the client, and codex
// writes $CODEX_HOME/auth.json itself once the user approves in a
// browser — the credential never transits clank. Success is exit 0
// PLUS a freshly-written auth.json: the CLI exits 0 on a delivered
// signal too, so the exit code alone proves nothing.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/acksell/clank/internal/agent"
)

// codexDeviceURLTimeout caps the wait for codex to print the
// verification URL + code (one round-trip to the OpenAI auth service).
const codexDeviceURLTimeout = 30 * time.Second

// codexDeviceCodeTTL is how long the one-time code stays valid; codex
// prints "(expires in 15 minutes)". Surfaced to clients as ExpiresAt.
const codexDeviceCodeTTL = 15 * time.Minute

// codexDeviceAuthTimeout bounds the background awaiter. One minute of
// slack past the code TTL lets codex report the expiry itself before
// clank gives up on the subprocess.
const codexDeviceAuthTimeout = codexDeviceCodeTTL + time.Minute

// codexDevicePollInterval is the status-poll cadence suggested to
// clients in DeviceFlowStart.Interval (seconds).
const codexDevicePollInterval = 2

// ErrCodexDeviceAuthUnavailable is returned when no codex login
// command is wired — the codex backend isn't enabled on this host.
var ErrCodexDeviceAuthUnavailable = errors.New("codex device auth is not available on this host (codex backend not enabled)")

// ansiEscapes matches CSI escape sequences; codex colors its login
// output even when piped, so parsing strips them first.
var ansiEscapes = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// codexDeviceURLRe / codexDeviceCodeRe pick the verification URL and
// the one-time code out of the login output. Both values sit alone on
// their own line (verified against codex 0.145.0):
//
//  1. Open this link in your browser and sign in to your account
//     https://auth.openai.com/codex/device
//  2. Enter this one-time code (expires in 15 minutes)
//     MKU9-QH4TO
var (
	codexDeviceURLRe  = regexp.MustCompile(`(?m)^\s*(https://\S+)\s*$`)
	codexDeviceCodeRe = regexp.MustCompile(`(?m)^\s*([A-Z0-9]{2,}(?:-[A-Z0-9]{2,})+)\s*$`)
)

// codexDeviceSession wraps one running `codex login --device-auth`
// subprocess: accumulated output plus exit observation. No PTY — the
// CLI is fully headless in this mode.
type codexDeviceSession struct {
	cmd *exec.Cmd

	mu  sync.Mutex
	out bytes.Buffer

	doneCh  chan struct{} // closed once the process is reaped
	waitErr error         // valid after doneCh closes
}

// Write appends subprocess output under mu; handed to cmd.Stdout and
// cmd.Stderr, so exec's copier goroutines are the only writers.
func (s *codexDeviceSession) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.out.Write(p)
}

// output returns the ANSI-stripped output accumulated so far.
func (s *codexDeviceSession) output() string {
	s.mu.Lock()
	raw := s.out.String()
	s.mu.Unlock()
	return ansiEscapes.ReplaceAllString(raw, "")
}

// close kills the subprocess; the Wait goroutine reaps it and closes
// doneCh. Safe to call multiple times and after natural exit.
func (s *codexDeviceSession) close() {
	_ = s.cmd.Process.Kill()
}

// outputTail squashes the output onto one line and keeps the end —
// enough context for an error message without dumping a banner.
func (s *codexDeviceSession) outputTail() string {
	text := strings.Join(strings.Fields(s.output()), " ")
	const max = 200
	if len(text) > max {
		text = "…" + text[len(text)-max:]
	}
	return text
}

// awaitLoginDetails polls the accumulated output until both the
// verification URL and the one-time code have appeared.
func (s *codexDeviceSession) awaitLoginDetails(ctx context.Context) (verificationURL, userCode string, err error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		text := s.output()
		urlMatch := codexDeviceURLRe.FindStringSubmatch(text)
		codeMatch := codexDeviceCodeRe.FindStringSubmatch(text)
		if urlMatch != nil && codeMatch != nil {
			return urlMatch[1], codeMatch[1], nil
		}
		select {
		case <-s.doneCh:
			return "", "", fmt.Errorf("codex login exited before printing the sign-in code: %s", s.outputTail())
		default:
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-ticker.C:
		}
	}
}

// fileStamp captures enough of a file's identity to detect a rewrite.
type fileStamp struct {
	exists  bool
	modTime time.Time
	size    int64
}

func stampFile(path string) fileStamp {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{exists: true, modTime: fi.ModTime(), size: fi.Size()}
}

// SetCodexLoginCommand wires the argv used to run the pinned codex
// CLI's device login (provisioning the tools on first use). Wired at
// service construction when the codex backend is enabled; nil keeps
// StartDeviceFlow returning ErrCodexDeviceAuthUnavailable.
func (a *AuthManager) SetCodexLoginCommand(f func(ctx context.Context) ([]string, error)) {
	a.codexLogin = f
}

// EnableCodexCLIFallback lets ListProviders report the machine's own
// codex CLI login ($CODEX_HOME/auth.json) as a connected subscription
// provider when clank didn't run the ceremony itself. A deployment
// decision like EnableClaudeCLIFallback: the local laptop provisioner
// enables it — the adapter inherits the host environment there and
// uses that login anyway — while sandboxes keep connection state
// explicit. Presence-only: the credential is never read. Call once at
// wiring time, before the manager serves requests.
func (a *AuthManager) EnableCodexCLIFallback() { a.codexCLIAuth = true }

// startCodexDeviceFlow spawns the codex device login, waits for it to
// print the verification URL + one-time code, and registers a flow
// whose background awaiter watches the subprocess to completion.
func (a *AuthManager) startCodexDeviceFlow(ctx context.Context) (agent.DeviceFlowStart, error) {
	if a.codexLogin == nil {
		return agent.DeviceFlowStart{}, ErrCodexDeviceAuthUnavailable
	}
	argv, err := a.codexLogin(ctx)
	if err != nil {
		return agent.DeviceFlowStart{}, fmt.Errorf("resolve codex login command: %w", err)
	}
	if len(argv) == 0 {
		return agent.DeviceFlowStart{}, errors.New("codex login command resolved to an empty argv")
	}

	// Pre-create the codex home: the CLI warns (and skips shell-alias
	// setup) when the directory is missing.
	codexHome := a.codexHome()
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		return agent.DeviceFlowStart{}, fmt.Errorf("create codex home: %w", err)
	}
	authBefore := stampFile(a.codexAuthJSONPath())

	sess := &codexDeviceSession{
		cmd:    exec.Command(argv[0], argv[1:]...),
		doneCh: make(chan struct{}),
	}
	// Fresh sign-in only: an inherited API key could satisfy codex's
	// auth resolution and short-circuit the ceremony. CODEX_HOME is
	// pinned so the login lands where the adapter's app-server child
	// and the status probes look.
	sess.cmd.Env = scrubEnv(os.Environ(), EnvCodexAPIKey, EnvOpenAIAPIKey, EnvCodexHome)
	sess.cmd.Env = append(sess.cmd.Env, EnvCodexHome+"="+codexHome)
	sess.cmd.Stdout = sess
	sess.cmd.Stderr = sess
	if err := sess.cmd.Start(); err != nil {
		return agent.DeviceFlowStart{}, fmt.Errorf("start codex login: %w", err)
	}
	go func() {
		sess.waitErr = sess.cmd.Wait()
		close(sess.doneCh)
	}()

	urlCtx, cancelURL := context.WithTimeout(ctx, codexDeviceURLTimeout)
	defer cancelURL()
	verificationURL, userCode, err := sess.awaitLoginDetails(urlCtx)
	if err != nil {
		sess.close()
		return agent.DeviceFlowStart{}, fmt.Errorf("await codex sign-in code: %w", err)
	}

	// Independent lifetime: approval can arrive minutes later, well
	// past the request ctx's deadline.
	awaitCtx, cancelAwait := context.WithCancel(context.Background())
	flowID := ulid.Make().String()
	a.flowMu.Lock()
	a.flows[flowID] = &flowState{state: agent.DeviceFlowPending, cancel: cancelAwait}
	a.flowMu.Unlock()

	go func() {
		defer cancelAwait() // prevent context leak on non-CancelFlow exits
		a.awaitCodexDeviceAuth(awaitCtx, flowID, sess, authBefore)
	}()

	return agent.DeviceFlowStart{
		FlowID:          flowID,
		UserCode:        userCode,
		VerificationURL: verificationURL,
		ExpiresAt:       time.Now().Add(codexDeviceCodeTTL),
		Interval:        codexDevicePollInterval,
	}, nil
}

// awaitCodexDeviceAuth watches the login subprocess to completion and
// drives the flow to its terminal state. Owns process teardown on
// every exit path.
func (a *AuthManager) awaitCodexDeviceAuth(ctx context.Context, flowID string, sess *codexDeviceSession, authBefore fileStamp) {
	defer sess.close()

	waitCtx, cancel := context.WithTimeout(ctx, codexDeviceAuthTimeout)
	defer cancel()
	select {
	case <-sess.doneCh:
	case <-waitCtx.Done():
		sess.close()
		<-sess.doneCh
		if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			a.finishFlowIfActive(flowID, agent.DeviceFlowExpired, "sign-in not completed before the one-time code expired")
		}
		// Cancellation: CancelFlow already recorded the canceled state.
		return
	}

	if sess.waitErr != nil {
		a.failFlowIfActive(flowID, "codex login failed: "+sess.outputTail())
		return
	}
	authAfter := stampFile(a.codexAuthJSONPath())
	if !authAfter.exists || authAfter == authBefore {
		// Exit 0 without a (re)written auth.json — the CLI exits 0 on a
		// delivered signal, so treat this as not signed in.
		a.failFlowIfActive(flowID, "codex login ended without completing sign-in")
		return
	}

	if err := a.setOpenAIChatGPTConnected(); err != nil {
		a.failFlowIfActive(flowID, "record chatgpt connection: "+err.Error())
		return
	}
	a.transition(flowID, agent.DeviceFlowAuthorized, "")
	if a.onOpenAICredential != nil {
		a.onOpenAICredential()
	}
	a.transition(flowID, agent.DeviceFlowSuccess, "")
}

// scrubEnv returns env without any of the named variables.
func scrubEnv(env []string, names ...string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		drop := false
		for _, name := range names {
			if strings.HasPrefix(kv, name+"=") {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}
