package preview

import (
	"bytes"
	"sync"
)

// ringBuf is a fixed-capacity append buffer that overwrites old bytes
// on overflow. Used to capture dev-server stdout/stderr so a status
// endpoint can return the last N KiB without an unbounded memory leak
// from a chatty Metro process.
//
// All ANSI CSI sequences ("ESC [ ...") are stripped on write — Metro
// emits color codes liberally and the consumer (mobile UI / curl) does
// not render them. Stripping at write time keeps reads cheap and the
// buffer's "last N bytes" guarantee meaningful (each retained byte is
// a real character, not a noise prefix).
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

// Write appends p (after ANSI stripping), discarding the oldest bytes
// once the buffer is full. Always returns (len(p), nil) — the
// io.Writer contract requires we report the unstripped count so callers
// driving a Copy loop don't loop forever.
func (r *ringBuf) Write(p []byte) (int, error) {
	stripped := stripANSI(p)
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

// stripANSI removes CSI escape sequences ("ESC[…m" and friends). Keeps
// every other byte verbatim, including non-ASCII. This is the only
// transformation applied to dev-server output before it lands in the
// ring; if we ever want to strip OSC or other escape families we'd
// extend here.
func stripANSI(p []byte) []byte {
	if !bytes.ContainsRune(p, 0x1b) {
		return p
	}
	out := make([]byte, 0, len(p))
	for i := 0; i < len(p); i++ {
		if p[i] != 0x1b || i+1 >= len(p) || p[i+1] != '[' {
			out = append(out, p[i])
			continue
		}
		// Skip "ESC [" and consume until a final byte in the 0x40-0x7E
		// range terminates the sequence.
		i += 1 // points at '['
		for j := i + 1; j < len(p); j++ {
			b := p[j]
			if b >= 0x40 && b <= 0x7e {
				i = j
				break
			}
			// Unterminated sequence at end of buffer — drop the rest.
			if j == len(p)-1 {
				return out
			}
		}
	}
	return out
}
