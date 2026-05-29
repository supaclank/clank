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

func TestStripANSI_NoEscape_PassThrough(t *testing.T) {
	t.Parallel()
	in := []byte("plain text, no escape")
	out := stripANSI(in)
	if !bytes.Equal(in, out) {
		t.Fatalf("stripANSI on plain input mutated bytes: in=%q out=%q", in, out)
	}
}

func TestStripANSI_UnterminatedSequenceDropped(t *testing.T) {
	t.Parallel()
	// "\x1b[2" lacks the final byte (e.g. 'm') — genuinely unterminated.
	// stripANSI should drop everything from ESC onward.
	got := string(stripANSI([]byte("hello\x1b[2")))
	want := "hello"
	if got != want {
		t.Fatalf("stripANSI unterminated = %q, want %q", got, want)
	}
}
