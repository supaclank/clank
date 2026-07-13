package webpreview

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"syscall"
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
			if mode == "toolong" {
				// A single line past the pump's 1 MiB scan buffer, no
				// newline: Scan() fails with ErrTooLong while this
				// process is still very much alive (it stays blocked
				// writing, exactly like a real wedged clank-voice).
				fmt.Fprint(out, strings.Repeat("x", 2<<20))
				out.Flush()
				continue
			}
			n += len(payload)
			emit(`{"type":"partial","text":"heard %d"}`, n)
		case frameEnd:
			emit(`{"type":"final","text":"total %d bytes"}`, n)
			if mode == "quickexit" {
				os.Exit(0) // races cmd.Wait() against the parent's still-unread final line
			}
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

// TestSherpaEngineCloseUnblocksOnStalledConsumer pins the fix for a
// deadlock: deliver held sinkMu while blocking on a full, undrained
// result channel for a Final/Err result, and Close's detach() needed
// the same lock — so a stalled reader wedged Close forever along with
// the engine's serialization semaphore.
func TestSherpaEngineCloseUnblocksOnStalledConsumer(t *testing.T) {
	e := fakeEngine(t, "ok")
	defer e.Close()

	s, err := e.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ch := s.(*sherpaSession).ch

	// Fill the result buffer without draining it, so the upcoming final
	// has nowhere to land.
	for i := 0; i < cap(ch); i++ {
		if err := s.Feed([]byte{byte(i)}); err != nil {
			t.Fatalf("Feed %d: %v", i, err)
		}
	}
	time.Sleep(200 * time.Millisecond) // let the pump fill the buffer
	if err := s.End(); err != nil {
		t.Fatalf("End: %v", err)
	}
	time.Sleep(200 * time.Millisecond) // let the pump attempt (and block on) the final

	done := make(chan error, 1)
	go func() { done <- s.Close() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("Close deadlocked behind a stalled consumer")
	}
}

// TestSherpaEnginePumpKillsProcessOnScanError pins the fix for a leak:
// when Scan() fails on something other than clean EOF (e.g. a
// too-long line), the pump used to mark the process dead and let
// ensureProc spawn a replacement without ever killing the original —
// which just sits there, still alive, holding its ~700 MB mmap.
func TestSherpaEnginePumpKillsProcessOnScanError(t *testing.T) {
	e := fakeEngine(t, "toolong")
	defer e.Close()

	s, err := e.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Feed(make([]byte, 10)); err != nil {
		t.Logf("Feed returned error (acceptable): %v", err)
	}

	proc := s.(*sherpaSession).proc
	select {
	case <-proc.dead:
	case <-time.After(3 * time.Second):
		t.Fatalf("pump never marked the process dead after the oversized line")
	}

	// Signal(0) is a liveness probe, not a real signal; it fails once the
	// process has actually been killed and reaped (unlike reading
	// cmd.ProcessState directly, which races with the Wait() goroutine).
	deadline := time.Now().Add(3 * time.Second)
	for proc.cmd.Process.Signal(syscall.Signal(0)) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("clank-voice process was never reaped after the scan error — leaked")
		}
		time.Sleep(20 * time.Millisecond)
	}

	_ = s.Close()
}

// TestSherpaEnginePumpReadsFinalBeforeReap pins the fix for a race:
// cmd.Wait() used to run in its own goroutine started right alongside
// pump(), racing pump's stdout reads (os/exec's StdoutPipe doc: Wait
// must not run until all reads complete, since Wait closes the pipe on
// exit). A process exiting immediately after writing its final line
// could have that line truncated by the close landing mid-read.
func TestSherpaEnginePumpReadsFinalBeforeReap(t *testing.T) {
	for i := 0; i < 30; i++ {
		e := fakeEngine(t, "quickexit")
		s, err := e.Open(context.Background())
		if err != nil {
			t.Fatalf("iter %d: Open: %v", i, err)
		}
		if err := s.Feed(make([]byte, 5)); err != nil {
			t.Fatalf("iter %d: Feed: %v", i, err)
		}
		if r := recvResult(t, s); r.Text != "heard 5" {
			t.Fatalf("iter %d: partial = %+v, want 'heard 5'", i, r)
		}
		if err := s.End(); err != nil {
			t.Logf("iter %d: End returned error (acceptable, process may already be exiting): %v", i, err)
		}
		if r := recvResult(t, s); !r.Final || r.Text != "total 5 bytes" {
			t.Fatalf("iter %d: final = %+v, want final 'total 5 bytes' (lost/truncated by racing Wait?)", i, r)
		}
		_ = s.Close()
		e.Close()
	}
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
