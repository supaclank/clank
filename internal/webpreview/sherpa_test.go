package webpreview

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"testing"
	"time"
)

// fakeVoiceEnvVar switches this test binary into "fake clank-voice"
// mode when re-executed by SherpaEngine (the exec.Command(os.Args[0],
// "-test.run=TestHelperFakeVoice$") pattern): the helper speaks the
// real stdin/stdout protocol so the driver is tested end to end
// without cgo, models, or the voice-engine module.
const fakeVoiceEnvVar = "WEBPREVIEW_FAKE_VOICE_MODE"

func TestHelperFakeVoice(t *testing.T) {
	mode := os.Getenv(fakeVoiceEnvVar)
	if mode == "" {
		t.Skip("helper process for SherpaEngine tests; not a real test")
	}
	runFakeVoice(mode)
	os.Exit(0) // never let the test framework print PASS onto the protocol stream
}

func runFakeVoice(mode string) {
	out := bufio.NewWriter(os.Stdout)
	emit := func(format string, args ...any) {
		fmt.Fprintf(out, format+"\n", args...)
		out.Flush()
	}
	emit(`{"type":"ready"}`)
	in := bufio.NewReader(os.Stdin)
	var n int
	for {
		typ, payload, err := readFrame(in)
		if err != nil {
			return
		}
		switch typ {
		case frameAudio:
			if mode == "crash" {
				os.Exit(3)
			}
			n += len(payload)
			emit(`{"type":"partial","text":"heard %d"}`, n)
		case frameEnd:
			emit(`{"type":"final","text":"total %d bytes"}`, n)
			n = 0
		case frameCancel:
			n = 0
		}
	}
}

func fakeEngine(t *testing.T, mode string) *SherpaEngine {
	t.Helper()
	t.Setenv(fakeVoiceEnvVar, mode)
	return &SherpaEngine{
		Bin:          os.Args[0],
		Args:         []string{"-test.run=TestHelperFakeVoice$"},
		ReadyTimeout: 10 * time.Second,
		Log:          log.New(io.Discard, "", 0),
	}
}

func recvResult(t *testing.T, s Session) Result {
	t.Helper()
	select {
	case r, ok := <-s.Results():
		if !ok {
			t.Fatalf("results channel closed unexpectedly")
		}
		return r
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for a result")
		return Result{}
	}
}

func TestSherpaEnginePartialsAndFinals(t *testing.T) {
	e := fakeEngine(t, "ok")
	defer e.Close()

	s, err := e.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if err := s.Feed(make([]byte, 100)); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if r := recvResult(t, s); r.Final || r.Text != "heard 100" {
		t.Fatalf("partial 1 = %+v, want cumulative 'heard 100'", r)
	}
	if err := s.Feed(make([]byte, 50)); err != nil {
		t.Fatalf("Feed 2: %v", err)
	}
	if r := recvResult(t, s); r.Final || r.Text != "heard 150" {
		t.Fatalf("partial 2 = %+v, want 'heard 150'", r)
	}
	if err := s.End(); err != nil {
		t.Fatalf("End: %v", err)
	}
	if r := recvResult(t, s); !r.Final || r.Text != "total 150 bytes" || r.Err != nil {
		t.Fatalf("final = %+v, want final 'total 150 bytes'", r)
	}

	// The session survives across utterances; the counter reset after
	// end proves per-utterance state in the engine process.
	if err := s.Feed(make([]byte, 7)); err != nil {
		t.Fatalf("Feed utterance 2: %v", err)
	}
	if r := recvResult(t, s); r.Text != "heard 7" {
		t.Fatalf("utterance 2 partial = %+v", r)
	}
	if err := s.End(); err != nil {
		t.Fatalf("End 2: %v", err)
	}
	if r := recvResult(t, s); !r.Final || r.Text != "total 7 bytes" {
		t.Fatalf("utterance 2 final = %+v", r)
	}
}

func TestSherpaEngineSerializesSessions(t *testing.T) {
	e := fakeEngine(t, "ok")
	defer e.Close()

	s1, err := e.Open(context.Background())
	if err != nil {
		t.Fatalf("Open s1: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := e.Open(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Open while s1 held = %v, want DeadlineExceeded", err)
	}

	if err := s1.Close(); err != nil {
		t.Fatalf("Close s1: %v", err)
	}
	s2, err := e.Open(context.Background())
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	_ = s2.Close()
}

func TestSherpaEngineRecoversFromCrash(t *testing.T) {
	e := fakeEngine(t, "crash")
	defer e.Close()

	s, err := e.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Feed(make([]byte, 10)); err != nil {
		// Feed may already observe the dead pipe — that's fine too.
		t.Logf("Feed returned error (acceptable): %v", err)
	}
	if r := recvResult(t, s); r.Err == nil {
		t.Fatalf("crash: want a Result with Err, got %+v", r)
	}
	_ = s.Close()

	// A fresh Open must respawn a healthy process.
	t.Setenv(fakeVoiceEnvVar, "ok")
	s2, err := e.Open(context.Background())
	if err != nil {
		t.Fatalf("Open after crash: %v", err)
	}
	defer s2.Close()
	if err := s2.Feed(make([]byte, 5)); err != nil {
		t.Fatalf("Feed after respawn: %v", err)
	}
	if r := recvResult(t, s2); r.Text != "heard 5" {
		t.Fatalf("respawned partial = %+v", r)
	}
}
