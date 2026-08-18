package preview

import (
	"sync"

	"github.com/charmbracelet/x/ansi"
)

// ringBuf is a fixed-capacity append buffer that overwrites old bytes
// on overflow. Used to capture dev-server stdout/stderr so a status
// endpoint can return the last N KiB without an unbounded memory leak
// from a chatty Metro process.
//
// Terminal escape sequences and controls are stripped on write. Newlines,
// carriage returns, and tabs remain so consumers can present readable logs.
type ringBuf struct {
	mu   sync.Mutex
	buf  []byte // capacity == cap; len grows up to cap then wraps via overwrite
	head int    // next-write index modulo cap; only meaningful when full
	full bool
}

func newRingBuf(capacity int) *ringBuf {
	if capacity <= 0 {
		panic("preview: ringBuf capacity must be positive")
	}
	return &ringBuf{buf: make([]byte, 0, capacity)}
}

// Write appends p (after sanitizing terminal controls), discarding the oldest
// bytes once the buffer is full. Always returns (len(p), nil) — the
// io.Writer contract requires we report the unstripped count so callers
// driving a Copy loop don't loop forever.
func (r *ringBuf) Write(p []byte) (int, error) {
	stripped := sanitizeTerminalOutput(p)
	r.mu.Lock()
	defer r.mu.Unlock()
	cap := cap(r.buf)
	for _, b := range stripped {
		if !r.full {
			if len(r.buf) < cap {
				r.buf = append(r.buf, b)
				if len(r.buf) == cap {
					r.full = true
					r.head = 0
				}
				continue
			}
			r.full = true
			r.head = 0
		}
		r.buf[r.head] = b
		r.head = (r.head + 1) % cap
	}
	return len(p), nil
}

// Snapshot returns the current contents in oldest-to-newest order.
// Allocates a fresh copy so the caller is free to mutate or hold it.
func (r *ringBuf) Snapshot() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		out := make([]byte, len(r.buf))
		copy(out, r.buf)
		return out
	}
	cap := cap(r.buf)
	out := make([]byte, cap)
	n := copy(out, r.buf[r.head:])
	copy(out[n:], r.buf[:r.head])
	return out
}

// sanitizeTerminalOutput removes escape sequences and non-layout controls.
func sanitizeTerminalOutput(p []byte) []byte {
	if !needsTerminalSanitizing(p) {
		return p
	}
	stripped := []byte(ansi.Strip(string(p)))
	out := stripped[:0]
	for _, b := range stripped {
		isLayoutControl := b == '\n' || b == '\r' || b == '\t'
		if (b < 0x20 && !isLayoutControl) || b == 0x7f {
			continue
		}
		out = append(out, b)
	}
	return out
}

// needsTerminalSanitizing reports whether p contains an escape sequence or a
// stray control byte, so the common chatty-stdout case (plain text) can skip
// ansi.Strip's allocation and copy on this hot capture path.
func needsTerminalSanitizing(p []byte) bool {
	for _, b := range p {
		if b == 0x1b || b == 0x7f {
			return true
		}
		isLayoutControl := b == '\n' || b == '\r' || b == '\t'
		if b < 0x20 && !isLayoutControl {
			return true
		}
	}
	return false
}
