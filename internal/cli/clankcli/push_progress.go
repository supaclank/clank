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

// pushUI renders push progress as a growing log: each phase shows a live
// spinner line while active, and is "committed" to a persistent "✓ …" line
// when the next phase starts — so completed steps stay on screen instead of
// being overwritten. A steady ticker animates the active line, so it never
// looks frozen even while the server commits or sessions export.
//
// It implements syncclient.PushObserver (Phase/UploadSized/UploadProgress)
// so PushCheckpoint drives it directly. A nil *pushUI is a safe no-op on
// every method, so non-interactive callers (autopush hooks) pass nil and
// nothing is drawn.
type pushUI struct {
	out    io.Writer
	remote string

	mu       sync.Mutex // guards the fields below AND serializes writes to out
	phase    string
	uploaded int64
	total    int64

	stop chan struct{}
	done chan struct{}
}

func newPushUI(out io.Writer, remote string) *pushUI {
	return &pushUI{out: out, remote: remote, phase: "Preparing"}
}

// Phase commits the line for the phase that just finished (persisting it as
// "✓ …") and starts the next. Synchronous, so even an instant phase (e.g. a
// fast server commit) still leaves its completed line on screen.
func (u *pushUI) Phase(name string) {
	if u == nil {
		return
	}
	u.mu.Lock()
	u.commitLocked()
	u.phase = name
	u.total, u.uploaded = 0, 0
	// Draw the new phase's line right away so there's no blank gap until the
	// next tick.
	fmt.Fprint(u.out, "\r\x1b[K"+liveLine(spinFrames[0], u.phase, u.remote, u.uploaded, u.total))
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

// spinFrames is a braille dot-cycle — a calm "series of dots" that clearly
// rotates (unlike a single static glyph).
var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// start launches the render loop that animates the active line. Pairs 1:1
// with finish.
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
				return
			case <-ticker.C:
				u.mu.Lock()
				fmt.Fprint(u.out, "\r\x1b[K"+liveLine(spinFrames[i%len(spinFrames)], u.phase, u.remote, u.uploaded, u.total))
				u.mu.Unlock()
			}
		}
	}()
}

// finish stops the ticker and clears the active (uncommitted) line — its
// result is printed by the caller. Earlier phases were committed on their
// transitions, so they remain on screen.
func (u *pushUI) finish() {
	if u == nil || u.stop == nil {
		return
	}
	close(u.stop)
	<-u.done
	u.mu.Lock()
	fmt.Fprint(u.out, "\r\x1b[K")
	u.mu.Unlock()
	u.stop = nil
}

// commitLocked persists the current phase as a "✓ …" line (with a trailing
// newline) when it has a committed form; phases without one (Preparing,
// the trailing session phase) are simply cleared/replaced. Caller holds mu.
func (u *pushUI) commitLocked() {
	if done := committedForm(u.phase, u.total); done != "" {
		fmt.Fprint(u.out, "\r\x1b[K"+done+"\n")
	}
}

// committedForm maps an in-progress phase to its persistent done line, or
// "" for phases the UI doesn't persist itself (the caller prints those).
func committedForm(phase string, total int64) string {
	check := styleOK.Render("✓") + " "
	switch phase {
	case syncclient.PhaseBuilding:
		return check + "Built bundle"
	case syncclient.PhaseUploading:
		return check + "Uploaded " + humanBytes(total)
	case syncclient.PhaseFinalizing:
		return check + "Saved checkpoint"
	default:
		return ""
	}
}

// liveLine renders the in-progress line for a phase: "<spinner> <phase> →
// <remote>  [bar]  up / total". The bar appears only once a size is known.
func liveLine(frame, phase, remote string, uploaded, total int64) string {
	line := styleOK.Render(frame) + " " + phase + styleDim.Render(" → "+remote)
	if total > 0 {
		line += "  " + renderBar(float64(uploaded)/float64(total), 24) + "  " + styleDim.Render(humanBytes(uploaded)+" / "+humanBytes(total))
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
