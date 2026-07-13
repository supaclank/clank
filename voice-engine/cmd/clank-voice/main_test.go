package main

import (
	"bufio"
	"errors"
	"strings"
	"testing"
)

// failWriter errors on every Write, simulating a parent that has closed
// or stopped reading stdout.
type failWriter struct{ err error }

func (w *failWriter) Write(p []byte) (int, error) { return 0, w.err }

func TestWriteMsgSucceeds(t *testing.T) {
	var sb strings.Builder
	w := bufio.NewWriter(&sb)
	if err := writeMsg(w, msg{Type: "final", Text: "hello"}); err != nil {
		t.Fatalf("writeMsg: %v", err)
	}
	if got, want := sb.String(), `{"type":"final","text":"hello"}`+"\n"; got != want {
		t.Fatalf("wrote %q, want %q", got, want)
	}
}

// TestWriteMsgReportsWriteFailure pins the fix for a silent hang: emit()
// used to discard Write/Flush errors, so a parent that stopped reading
// stdout (e.g. mid-shutdown) left the subprocess decoding audio into a
// void with no way to detect or recover from the broken pipe.
func TestWriteMsgReportsWriteFailure(t *testing.T) {
	wantErr := errors.New("broken pipe")
	w := bufio.NewWriter(&failWriter{err: wantErr})
	err := writeMsg(w, msg{Type: "final", Text: "hello"})
	if err == nil {
		t.Fatalf("writeMsg: want error on a failing writer, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("writeMsg error = %v, want it to wrap %v", err, wantErr)
	}
}
