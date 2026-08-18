package preview

import (
	"bytes"
	"testing"
)

func TestRingBufAppendsUntilCapacity(t *testing.T) {
	t.Parallel()
	r := newRingBuf(8)
	r.Write([]byte("abc"))
	r.Write([]byte("def"))
	if got, want := string(r.Snapshot()), "abcdef"; got != want {
		t.Fatalf("snapshot = %q, want %q", got, want)
	}
}

func TestRingBufOverwritesOldest(t *testing.T) {
	t.Parallel()
	r := newRingBuf(4)
	r.Write([]byte("abcd"))
	r.Write([]byte("ef"))
	if got, want := string(r.Snapshot()), "cdef"; got != want {
		t.Fatalf("snapshot = %q, want %q", got, want)
	}
}

func TestRingBufStripsANSI(t *testing.T) {
	t.Parallel()
	r := newRingBuf(64)
	// "\x1b[2mgrey\x1b[0m text" — two CSI sequences around "grey".
	r.Write([]byte("\x1b[2mgrey\x1b[0m text"))
	got := string(r.Snapshot())
	want := "grey text"
	if got != want {
		t.Fatalf("snapshot = %q, want %q (ANSI should be stripped)", got, want)
	}
}

func TestRingBufStripsTerminalControlSequences(t *testing.T) {
	t.Parallel()
	r := newRingBuf(64)
	r.Write([]byte("before\x1b]0;spoofed title\x07after\x07"))
	got := string(r.Snapshot())
	want := "beforeafter"
	if got != want {
		t.Fatalf("snapshot = %q, want %q (terminal controls should be stripped)", got, want)
	}
}

func TestSanitizeTerminalOutputPlainTextPassesThrough(t *testing.T) {
	t.Parallel()
	in := []byte("plain text, no escape")
	out := sanitizeTerminalOutput(in)
	if !bytes.Equal(in, out) {
		t.Fatalf("sanitizeTerminalOutput on plain input mutated bytes: in=%q out=%q", in, out)
	}
}

// TestSanitizeTerminalOutputPlainTextTakesFastPath pins the perf fix for the
// cubic finding on PR #261: plain text (the common case on this hot capture
// path) must return the same underlying array, not a stripped copy.
func TestSanitizeTerminalOutputPlainTextTakesFastPath(t *testing.T) {
	t.Parallel()
	in := []byte("plain text, no escape")
	out := sanitizeTerminalOutput(in)
	if &out[0] != &in[0] {
		t.Fatal("sanitizeTerminalOutput copied plain input instead of returning it unchanged")
	}
}

func TestSanitizeTerminalOutputStrayControlByteIsStripped(t *testing.T) {
	t.Parallel()
	// A stray BEL with no preceding ESC must still be stripped, even though
	// the ESC-absence fast path skips ansi.Strip.
	got := string(sanitizeTerminalOutput([]byte("before\x07after")))
	want := "beforeafter"
	if got != want {
		t.Fatalf("sanitizeTerminalOutput(%q) = %q, want %q", "before\x07after", got, want)
	}
}

// TestSanitizeTerminalOutputStripsC1Introducer pins the cubic finding on PR
// #261: the fast path only checked for ESC (0x1b), so an 8-bit C1 CSI byte
// (0x9b) with no literal ESC leaked its escape sequence into captured logs.
func TestSanitizeTerminalOutputStripsC1Introducer(t *testing.T) {
	t.Parallel()
	got := string(sanitizeTerminalOutput([]byte("a\x9b[31mred\x9b[0m")))
	want := "a31mred0m"
	if got != want {
		t.Fatalf("sanitizeTerminalOutput(%q) = %q, want %q", "a\x9b[31mred\x9b[0m", got, want)
	}
}

// TestSanitizeTerminalOutputPreservesUTF8ContinuationByteMatchingC1 pins the
// cubic finding on PR #261: 0x9b is both the CSI C1 introducer and a valid
// UTF-8 continuation byte (e.g. in 'ś', U+015B = 0xC5 0x9B). The byte-level
// C1 check must not treat that continuation byte as a raw escape
// introducer, or ansi.Strip corrupts otherwise-valid non-ASCII text.
func TestSanitizeTerminalOutputPreservesUTF8ContinuationByteMatchingC1(t *testing.T) {
	t.Parallel()
	in := []byte("stanśów")
	got := string(sanitizeTerminalOutput(in))
	want := "stanśów"
	if got != want {
		t.Fatalf("sanitizeTerminalOutput(%q) = %q, want %q (valid UTF-8 corrupted)", in, got, want)
	}
}

// TestSanitizeTerminalOutputStripsRawC1Controls pins the coderabbit/cubic
// finding on PR #261: 0x84 (IND) and 0x8d (RI) are C1 controls per ECMA-48
// that ansi.Strip doesn't recognize as escape introducers, but the doc
// comment promises all terminal controls are stripped. They must not reach
// the ring buffer as raw bytes.
func TestSanitizeTerminalOutputStripsRawC1Controls(t *testing.T) {
	t.Parallel()
	got := string(sanitizeTerminalOutput([]byte("before\x84middle\x8dafter")))
	want := "beforemiddleafter"
	if got != want {
		t.Fatalf("sanitizeTerminalOutput(%q) = %q, want %q", "before\x84middle\x8dafter", got, want)
	}
}

func TestSanitizeTerminalOutputDropsUnterminatedSequence(t *testing.T) {
	t.Parallel()
	// "\x1b[2" lacks the final byte (e.g. 'm') — genuinely unterminated.
	// sanitizeTerminalOutput should drop everything from ESC onward.
	got := string(sanitizeTerminalOutput([]byte("hello\x1b[2")))
	want := "hello"
	if got != want {
		t.Fatalf("sanitizeTerminalOutput unterminated = %q, want %q", got, want)
	}
}
