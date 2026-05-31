package clankcli

import (
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"

	syncclient "github.com/acksell/clank/pkg/sync/client"
)

// compile-time check: pushUI satisfies the progress observer contract.
var _ syncclient.PushObserver = (*pushUI)(nil)

// pushUI renders a single rewriting status line on a TTY — a spinner + the
// current phase + remote, plus a shaded byte bar while uploading. A steady
// ticker drives the redraw, so the line keeps animating even when no
// progress event has arrived for a while (e.g. while the server commits or
// sessions export) — that's what was missing before.
//
// It implements syncclient.PushObserver so PushCheckpoint feeds it
// directly, and Phase lets later legs (session sync) drive it too. A nil
// *pushUI is a safe no-op on every method, so non-interactive callers
// (autopush hooks) can pass it around without branching.
type pushUI struct {
	out    io.Writer
	remote string

	mu       sync.Mutex
	phase    string
	uploaded int64
	total    int64

	stop chan struct{}
	done chan struct{}
}

func newPushUI(out io.Writer, remote string) *pushUI {
	return &pushUI{out: out, remote: remote, phase: "Preparing"}
}

// Phase / UploadSized / UploadProgress implement syncclient.PushObserver;
// all are nil-safe and safe for concurrent use with the render loop.
func (u *pushUI) Phase(name string) {
	if u == nil {
		return
	}
	u.mu.Lock()
	u.phase = name
	u.mu.Unlock()
}

func (u *pushUI) UploadSized(total int64) {
	if u == nil {
		return
	}
	u.mu.Lock()
	u.total = total
	u.mu.Unlock()
}

func (u *pushUI) UploadProgress(uploaded int64) {
	if u == nil {
		return
	}
	u.mu.Lock()
	u.uploaded = uploaded
	u.mu.Unlock()
}

// clearBar drops the byte bar, for phases that aren't byte-tracked (e.g.
// session sync) so a stale checkpoint bar doesn't linger under them.
func (u *pushUI) clearBar() {
	if u == nil {
		return
	}
	u.mu.Lock()
	u.total, u.uploaded = 0, 0
	u.mu.Unlock()
}

// spinFrames is a braille dot-cycle — a calm "series of dots" that clearly
// rotates (unlike a single static glyph).
var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// start launches the render loop. Pairs 1:1 with finish.
func (u *pushUI) start() {
	if u == nil {
		return
	}
	u.stop = make(chan struct{})
	u.done = make(chan struct{})
	go func() {
		defer close(u.done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for i := 0; ; i++ {
			select {
			case <-u.stop:
				fmt.Fprint(u.out, "\r\x1b[K") // clear the line for the caller's summary
				return
			case <-ticker.C:
				u.draw(spinFrames[i%len(spinFrames)])
			}
		}
	}()
}

// finish stops the render loop and clears the line, blocking until the
// loop has cleared so the caller's next print starts on a clean line.
func (u *pushUI) finish() {
	if u == nil || u.stop == nil {
		return
	}
	close(u.stop)
	<-u.done
	u.stop = nil
}

func (u *pushUI) draw(frame string) {
	u.mu.Lock()
	phase, up, total := u.phase, u.uploaded, u.total
	u.mu.Unlock()
	fmt.Fprint(u.out, "\r\x1b[K"+pushLine(frame, phase, u.remote, up, total))
}

// pushLine builds the status line (pure, for tests): "<spinner> <phase> →
// <remote>  [bar]  up / total". The bar appears only once a size is known.
func pushLine(frame, phase, remote string, uploaded, total int64) string {
	line := styleOK.Render(frame) + " " + phase + styleDim.Render(" → "+remote)
	if total > 0 {
		pct := float64(uploaded) / float64(total)
		line += "  " + renderBar(pct, 24) + "  " + styleDim.Render(humanBytes(uploaded)+" / "+humanBytes(total))
	}
	return line
}

// renderBar draws a shaded bar like "[ ███▓░░░░ ]": full cells █, a single
// ▓ transition cell at the leading edge for the partial cell, and ░ for the
// remainder. The brackets and empty run are dimmed so the filled portion
// reads clearly without spending an accent colour.
func renderBar(pct float64, width int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 1 {
		pct = 1
	}
	exact := pct * float64(width)
	full := int(exact)
	if full > width {
		full = width
	}
	filled := strings.Repeat("█", full)
	written := full
	if written < width && exact-float64(full) > 0 {
		filled += "▓"
		written++
	}
	empty := strings.Repeat("░", width-written)
	return styleDim.Render("[ ") + filled + styleDim.Render(empty+" ]")
}

// remoteLabel renders a friendly host label for a gateway URL.
func remoteLabel(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Host
}

// humanBytes formats a byte count compactly (e.g. "57.8 MB").
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
