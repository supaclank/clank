package host

// PTY-relay for `claude setup-token`. Anthropic's documented path for
// generating a long-lived subscription token (Pro/Max). Spawning it in
// a pseudo-terminal on the sprite lets us drive the CLI's interactive
// flow from a remote UI: capture the authorize URL it prints, surface
// it to the user, accept the code they copy back from
// platform.claude.com, write it to the CLI's stdin, and capture the
// `sk-ant-oat01-…` token from stdout — all without the user needing a
// laptop.
//
// The CLI's stdout is heavily ANSI-formatted (cursor positioning splits
// the token across columns at some widths, splash-screen banners,
// spinner glyphs, etc). Parsing the raw byte stream is brittle to CLI
// version changes, so we feed it into vt10x — a virtual terminal
// emulator — and read the resulting screen contents like a human
// would. That keeps us robust to whatever rendering tricks the CLI
// uses, since we operate on the rendered display rather than the
// escape codes.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/hinshun/vt10x"
)

// setupTokenCols / setupTokenRows size the virtual terminal. Wide
// enough that the authorize URL (≈350 chars) fits on one screen row
// without terminal-driven wrap, tall enough that the post-submit
// "token created" + "Store this token securely" lines coexist with
// the earlier URL section.
const (
	setupTokenCols = 500
	setupTokenRows = 50
)

// setupTokenBinary is the CLI we spawn. Overridable for tests so we
// can substitute a fake binary that exercises the same I/O contract
// without needing real Anthropic auth.
var setupTokenBinary = "claude"

// Pattern anchors. Authorize URL has a stable prefix Anthropic
// publishes in their docs; long-lived tokens have a stable
// `sk-ant-oat01-` prefix per the same docs.
var (
	setupTokenURLPattern   = regexp.MustCompile(`https://claude\.com/cai/oauth/authorize\?\S+`)
	setupTokenTokenPattern = regexp.MustCompile(`sk-ant-oat01-[A-Za-z0-9_\-]+`)
)

// setupTokenSession is one in-flight `claude setup-token` run. The
// flow lifetime is: spawn → awaitURL → (surface URL to user) →
// submitCode → awaitToken → close. Cancellation at any point kills
// the subprocess and tears down the goroutine.
//
// Thread safety: termMu serializes vt10x access between the readLoop
// (writer) and screen() (reader). Everything else is single-flight
// from the AuthManager's perspective.
type setupTokenSession struct {
	cmd      *exec.Cmd
	ptmx     *os.File
	term     vt10x.Terminal
	termMu   sync.Mutex
	cancelFn context.CancelFunc
	doneCh   chan struct{} // closed when readLoop returns (process exited)
}

// startSetupToken spawns the CLI in a wide PTY and begins streaming
// its stdout into the virtual terminal. Returns once spawn succeeds
// — the caller drives the next state via awaitURL.
func startSetupToken(_ context.Context) (*setupTokenSession, error) {
	// Independent run context so the session's lifetime is gated by
	// close(), not by the caller's request context (which often
	// expires within seconds, far less than the OAuth flow takes).
	runCtx, cancel := context.WithCancel(context.Background())

	cmd := exec.CommandContext(runCtx, setupTokenBinary, "setup-token")
	// Strip any inherited Anthropic auth that would short-circuit the
	// setup-token flow or confuse it. The CLI's own auth-precedence
	// rules say env vars beat the OAuth fallback, so an existing
	// CLAUDE_CODE_OAUTH_TOKEN in the daemon's environment would make
	// setup-token "succeed" using the inherited token instead of
	// minting a fresh one.
	cmd.Env = scrubEnv(os.Environ(), EnvClaudeCodeOAuthToken, EnvAnthropicAPIKey, EnvAnthropicAuthToken)

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: setupTokenCols, Rows: setupTokenRows})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("pty.StartWithSize: %w", err)
	}

	term := vt10x.New(vt10x.WithSize(setupTokenCols, setupTokenRows))

	s := &setupTokenSession{
		cmd:      cmd,
		ptmx:     ptmx,
		term:     term,
		cancelFn: cancel,
		doneCh:   make(chan struct{}),
	}
	go s.readLoop()
	return s, nil
}

// readLoop pumps PTY bytes into the virtual terminal until EOF or
// PTY close. Closing doneCh on exit lets the awaiters detect "the
// process is gone" and bail out instead of polling forever.
func (s *setupTokenSession) readLoop() {
	defer close(s.doneCh)
	br := bufio.NewReader(s.ptmx)
	_ = s.parse(br)
}

// parse delegates to vt10x.Parse but routes the lock through termMu
// so screen() reads don't race the writer. We use Parse rather than
// io.Copy(term, ptmx) so multi-byte escape sequences land atomically
// even when the PTY returns partial reads.
func (s *setupTokenSession) parse(br *bufio.Reader) error {
	for {
		// vt10x.Parse blocks until it has a complete sequence; we
		// hold termMu only for the parse call, not the read, so
		// status pollers don't have to wait on PTY I/O.
		if _, err := br.Peek(1); err != nil {
			return err
		}
		s.termMu.Lock()
		err := s.term.Parse(br)
		s.termMu.Unlock()
		if err != nil {
			return err
		}
	}
}

// screen returns the rendered display content as a single string with
// trailing whitespace stripped per row. Cheap; safe to call as fast as
// the caller wants.
func (s *setupTokenSession) screen() string {
	s.termMu.Lock()
	defer s.termMu.Unlock()
	return s.term.String()
}

// awaitURL polls the rendered screen until the authorize URL appears,
// or ctx expires, or the subprocess exits prematurely.
func (s *setupTokenSession) awaitURL(ctx context.Context) (string, error) {
	return s.awaitPattern(ctx, setupTokenURLPattern, "authorize URL")
}

// awaitToken polls the rendered screen until the long-lived token
// appears. Called after submitCode.
func (s *setupTokenSession) awaitToken(ctx context.Context) (string, error) {
	return s.awaitPattern(ctx, setupTokenTokenPattern, "long-lived token")
}

const setupTokenPollInterval = 100 * time.Millisecond

// setupTokenSubmitDelay separates the pasted code from the Enter
// keystroke in submitCode (see the comment there). ~300ms+ is enough on
// Claude Code 2.1.x; 400ms leaves headroom.
var setupTokenSubmitDelay = 400 * time.Millisecond

func (s *setupTokenSession) awaitPattern(ctx context.Context, re *regexp.Regexp, label string) (string, error) {
	ticker := time.NewTicker(setupTokenPollInterval)
	defer ticker.Stop()

	if m := re.FindString(s.screen()); m != "" {
		return m, nil
	}
	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("timed out waiting for %s: %w", label, ctx.Err())
		case <-s.doneCh:
			// Process exited. One last check in case the match
			// landed in the same Parse cycle that triggered EOF.
			if m := re.FindString(s.screen()); m != "" {
				return m, nil
			}
			return "", fmt.Errorf("setup-token exited before %s appeared", label)
		case <-ticker.C:
			if m := re.FindString(s.screen()); m != "" {
				return m, nil
			}
		}
	}
}

// submitCode delivers the user's pasted authorization code to the
// running setup-token subprocess and presses Enter so the CLI exchanges
// it. The code and the Enter go in TWO writes separated by
// setupTokenSubmitDelay: Claude Code's Ink TUI debounces a code+CR sent
// in a single write as one paste event and swallows the trailing CR, so
// the code lands in the "Paste code here" field but is never submitted
// and the whole flow times out waiting for a token. A separate Enter
// keystroke after the gap registers as a real "submit".
func (s *setupTokenSession) submitCode(code string) error {
	if _, err := s.ptmx.Write([]byte(code)); err != nil {
		return err
	}
	timer := time.NewTimer(setupTokenSubmitDelay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-s.doneCh:
		return fmt.Errorf("setup-token exited during submit")
	}
	if _, err := s.ptmx.Write([]byte("\r")); err != nil {
		return err
	}
	return nil
}

// close tears the session down: cancels the run context (which the
// exec.CommandContext propagates to the subprocess as SIGKILL),
// closes the PTY, and waits for the readLoop goroutine to return.
// Idempotent.
func (s *setupTokenSession) close() {
	s.cancelFn()
	_ = s.ptmx.Close()
	// Belt-and-braces kill in case CommandContext hasn't kicked in
	// yet (the subprocess might still be in setup).
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	<-s.doneCh
	// Reap the zombie. Ignore the error — we don't care whether the
	// CLI exited cleanly, we're cleaning up.
	_ = s.cmd.Wait()
}

// ErrSetupTokenUnavailable is returned when the `claude` CLI is
// missing on PATH. Mux maps this to a 503.
var ErrSetupTokenUnavailable = errors.New("claude CLI not installed; cannot run setup-token")
