package webpreview

import (
	"context"
	"strings"
	"testing"
	"time"
)

// runExecUtterance drives one utterance through an ExecEngine session
// and returns its final Result.
func runExecUtterance(t *testing.T, cmdline string, pcm []byte) Result {
	t.Helper()
	e := &ExecEngine{Cmdline: cmdline}
	s, err := e.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if len(pcm) > 0 {
		if err := s.Feed(pcm); err != nil {
			t.Fatalf("Feed: %v", err)
		}
	}
	if err := s.End(); err != nil {
		t.Fatalf("End: %v", err)
	}
	select {
	case r := <-s.Results():
		if !r.Final {
			t.Fatalf("exec engine emitted a non-final result: %+v", r)
		}
		return r
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for final")
		return Result{}
	}
}

// The exec engine's contract with user-configured commands: a 16 kHz
// mono s16le WAV on stdin, transcript on stdout. `head -c 4` proves the
// stdin really is a RIFF container (whisper-cli et al. sniff exactly
// this), and the echo proves stdout capture + trimming.
func TestExecEngineWAVOnStdin(t *testing.T) {
	t.Parallel()
	r := runExecUtterance(t, `head -c 4; echo`, []byte{0, 0, 0, 0})
	if r.Err != nil || r.Text != "RIFF" {
		t.Fatalf("result = %+v, want RIFF (WAV header)", r)
	}
}

func TestExecEngineTrimsOutput(t *testing.T) {
	t.Parallel()
	r := runExecUtterance(t, `cat > /dev/null; printf '  hello world \n\n'`, make([]byte, 320))
	if r.Err != nil || r.Text != "hello world" {
		t.Fatalf("result = %+v, want 'hello world'", r)
	}
}

func TestExecEngineFailureCarriesStderr(t *testing.T) {
	t.Parallel()
	r := runExecUtterance(t, `cat > /dev/null; echo "model not found" >&2; exit 3`, make([]byte, 32))
	if r.Err == nil || !strings.Contains(r.Err.Error(), "model not found") {
		t.Fatalf("result = %+v, want error carrying stderr detail", r)
	}
}

// An empty utterance (push-to-talk tapped, no audio) must resolve to an
// empty final without running the command at all — a command that would
// fail proves the skip.
func TestExecEngineEmptyUtteranceSkipsCommand(t *testing.T) {
	t.Parallel()
	r := runExecUtterance(t, `exit 9`, nil)
	if r.Err != nil || r.Text != "" {
		t.Fatalf("empty utterance = %+v, want empty final with no error", r)
	}
}

func TestExecEngineCancelDiscardsBuffer(t *testing.T) {
	t.Parallel()
	e := &ExecEngine{Cmdline: `wc -c | tr -d ' '`}
	s, err := e.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	_ = s.Feed(make([]byte, 1000))
	_ = s.Cancel()
	_ = s.Feed(make([]byte, 100))
	_ = s.End()
	r := <-s.Results()
	// 100 bytes PCM + 44-byte WAV header.
	if r.Err != nil || r.Text != "144" {
		t.Fatalf("post-cancel result = %+v, want byte count 144 (canceled audio dropped)", r)
	}
}

func TestWAVHeaderShape(t *testing.T) {
	t.Parallel()
	pcm := make([]byte, 1600) // 50 ms
	wav := wavFromPCM(pcm)
	if len(wav) != 44+len(pcm) {
		t.Fatalf("wav length = %d, want %d", len(wav), 44+len(pcm))
	}
	if string(wav[:4]) != "RIFF" || string(wav[8:16]) != "WAVEfmt " || string(wav[36:40]) != "data" {
		t.Fatalf("malformed header: % x", wav[:44])
	}
}
