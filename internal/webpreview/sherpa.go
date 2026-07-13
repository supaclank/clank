package webpreview

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// SherpaEngine drives a clank-voice subprocess (voice-engine module):
// sherpa-onnx with Silero VAD + the same Parakeet model clank-mobile
// runs on-device. The subprocess is spawned once and kept warm — the
// model load is the expensive part (~670 MB of weights), so paying it
// per utterance or per WebSocket would wreck push-to-talk latency.
//
// The cgo/onnxruntime dependency lives entirely in the voice-engine
// module; this driver is pure Go, so clank's CGO_ENABLED=0 cross-builds
// (e.g. the fly.io clank-host artifact) are unaffected.
//
// Sessions are serialized: one model process, one utterance stream at a
// time. Open blocks until the previous session Closes.
type SherpaEngine struct {
	// Bin is the clank-voice executable path.
	Bin string
	// Args are passed verbatim (production: --models <dir>). Split out
	// so tests can substitute a fake binary with its own flags.
	Args []string
	// ReadyTimeout caps the model-load wait after spawn. Zero means
	// defaultReadyTimeout.
	ReadyTimeout time.Duration
	Log          *log.Logger

	initOnce sync.Once
	sem      chan struct{} // capacity 1: session serialization

	procMu sync.Mutex
	proc   *voiceProc
	closed bool
}

// defaultReadyTimeout allows for a cold Parakeet load on a busy laptop.
const defaultReadyTimeout = 180 * time.Second

// deliverBlockTimeout bounds how long deliver waits to hand a load-bearing
// Final/Err result to a stalled consumer before dropping it. Held under
// sinkMu, so unbounded blocking here would deadlock against Close's detach.
const deliverBlockTimeout = 1 * time.Second

// NewSherpaEngine returns an engine driving bin with the standard
// production arguments.
func NewSherpaEngine(bin, modelsDir string, lg *log.Logger) *SherpaEngine {
	return &SherpaEngine{Bin: bin, Args: []string{"--models", modelsDir}, Log: lg}
}

func (e *SherpaEngine) Describe() string {
	return "sherpa-onnx (" + ModelSlug + ") via " + filepath.Base(e.Bin)
}

func (e *SherpaEngine) init() {
	e.initOnce.Do(func() {
		e.sem = make(chan struct{}, 1)
		if e.Log == nil {
			e.Log = log.Default()
		}
	})
}

func (e *SherpaEngine) Open(ctx context.Context) (Session, error) {
	e.init()
	select {
	case e.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// The caller's ctx bounds only the busy-slot wait above. Model
	// spawn/readiness gets the engine's own budget: a first session
	// racing a cold prewarm (cold page cache, IO contention) must wait
	// for the load, not surface a spurious "busy" after ten seconds.
	timeout := e.ReadyTimeout
	if timeout == 0 {
		timeout = defaultReadyTimeout
	}
	rctx, cancel := context.WithTimeout(context.Background(), timeout)
	proc, err := e.ensureProc(rctx)
	cancel()
	if err != nil {
		<-e.sem
		return nil, err
	}
	s := &sherpaSession{engine: e, proc: proc, ch: make(chan Result, 8)}
	proc.attach(s.ch)
	return s, nil
}

// Prewarm starts loading the model in the background so the first
// push-to-talk doesn't pay the multi-second load. A session opened
// mid-load simply waits on the spawn lock and proceeds when ready.
func (e *SherpaEngine) Prewarm() {
	e.init()
	go func() {
		if _, err := e.ensureProc(context.Background()); err != nil {
			e.Log.Printf("webpreview: voice prewarm: %v", err)
		}
	}()
}

// Close tears down the warm model process. Implements io.Closer so the
// CLI can release ~1 GB of RSS on preview shutdown.
func (e *SherpaEngine) Close() error {
	e.procMu.Lock()
	defer e.procMu.Unlock()
	e.closed = true
	if e.proc != nil {
		e.proc.stop(3 * time.Second)
		e.proc = nil
	}
	return nil
}

// ensureProc returns the live subprocess, spawning (and waiting for its
// ready line) if needed. The readiness wait happens with procMu released
// — holding it across a wait of up to ReadyTimeout would make Close
// (Ctrl+C) block for the same duration behind a concurrent spawn.
func (e *SherpaEngine) ensureProc(ctx context.Context) (*voiceProc, error) {
	e.procMu.Lock()
	if e.closed {
		e.procMu.Unlock()
		return nil, fmt.Errorf("engine closed")
	}
	readyTimeout := e.ReadyTimeout
	if readyTimeout == 0 {
		readyTimeout = defaultReadyTimeout
	}
	if e.proc != nil && !e.proc.isDead() {
		p := e.proc
		e.procMu.Unlock()
		return waitReady(ctx, p, readyTimeout)
	}
	e.proc = nil

	cmd := exec.Command(e.Bin, e.Args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		e.procMu.Unlock()
		return nil, fmt.Errorf("clank-voice stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		e.procMu.Unlock()
		return nil, fmt.Errorf("clank-voice stdout: %w", err)
	}
	cmd.Stderr = &logWriter{log: e.Log, prefix: "clank-voice: "}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		e.procMu.Unlock()
		return nil, fmt.Errorf("start %s: %w", e.Bin, err)
	}
	p := &voiceProc{
		cmd:   cmd,
		stdin: stdin,
		dead:  make(chan struct{}),
		ready: make(chan struct{}),
		log:   e.Log,
	}
	// Wait must not run concurrently with pump's stdout reads: it can close
	// the pipe as soon as the process exits, truncating buffered output
	// (os/exec.Cmd.StdoutPipe doc). Reap only after pump has fully drained it.
	go func() {
		p.pump(stdout)
		_ = cmd.Wait()
	}()

	// Published before readiness so a racing Close (see the closed check
	// above) can find and kill a still-loading process instead of leaking it.
	e.proc = p
	e.procMu.Unlock()

	return waitReady(ctx, p, readyTimeout)
}

// waitReady blocks (unlocked) until p signals ready, dies, or timeout/ctx
// expires, killing p on any non-ready outcome.
func waitReady(ctx context.Context, p *voiceProc, timeout time.Duration) (*voiceProc, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop() // time.After would leak its timer until it fires
	select {
	case <-p.ready:
		return p, nil
	case <-p.dead:
		return nil, fmt.Errorf("clank-voice exited before ready (see log)")
	case <-timer.C:
		go p.stop(2 * time.Second) // failure path — don't hold the caller for the grace window
		return nil, fmt.Errorf("clank-voice not ready after %s", timeout)
	case <-ctx.Done():
		go p.stop(2 * time.Second)
		return nil, ctx.Err()
	}
}

// voiceProc is one live clank-voice subprocess.
type voiceProc struct {
	cmd   *exec.Cmd
	dead  chan struct{}
	ready chan struct{}
	log   *log.Logger

	writeMu sync.Mutex
	stdin   io.WriteCloser

	sinkMu sync.Mutex
	sink   chan<- Result // current session's channel; nil between sessions
}

func (p *voiceProc) isDead() bool {
	select {
	case <-p.dead:
		return true
	default:
		return false
	}
}

// stop asks the process to exit cleanly — closing stdin EOFs its frame
// loop — and escalates to SIGKILL only after grace. The gentle path is
// load-bearing, not politeness: SIGKILLing a process while it faults in
// ~700 MB of mmap'd onnx weights can leave it stuck in uninterruptible
// kernel exit on macOS, and every successor that maps the same
// libraries wedges behind it until reboot (observed during bring-up).
func (p *voiceProc) stop(grace time.Duration) {
	p.writeMu.Lock()
	_ = p.stdin.Close()
	p.writeMu.Unlock()
	select {
	case <-p.dead:
	case <-time.After(grace):
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
	}
}

// attach routes subsequent engine output to ch.
func (p *voiceProc) attach(ch chan<- Result) {
	p.sinkMu.Lock()
	p.sink = ch
	p.sinkMu.Unlock()
}

// detach stops routing and returns the channel for the caller to close.
func (p *voiceProc) detach() {
	p.sinkMu.Lock()
	p.sink = nil
	p.sinkMu.Unlock()
}

// deliver routes one Result to the attached session. The send happens
// under sinkMu so detach() can't race it: Close waits for any in-flight
// send to land before the channel is closed.
func (p *voiceProc) deliver(r Result) {
	p.sinkMu.Lock()
	defer p.sinkMu.Unlock()
	if p.sink == nil {
		return // engine output with no session listening (e.g. late final after Close)
	}
	select {
	case p.sink <- r:
	default:
		// A stalled consumer must not wedge the stdout pump; partials
		// are cumulative so dropping one loses nothing durable.
		if !r.Final && r.Err == nil {
			return
		}
		// Finals/errors are load-bearing, but blocking here holds sinkMu
		// and would deadlock against Close's detach(); give the reader a
		// grace window, then drop.
		select {
		case p.sink <- r:
		case <-time.After(deliverBlockTimeout):
			p.log.Printf("webpreview: deliver blocked past %s, dropping result: %+v", deliverBlockTimeout, r)
		}
	}
}

func (p *voiceProc) writeFrame(typ byte, payload []byte) error {
	if p.isDead() {
		return fmt.Errorf("voice engine process died")
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return writeFrame(p.stdin, typ, payload)
}

// pump translates clank-voice stdout lines into Results for the
// attached session. Exits when stdout closes (process death).
func (p *voiceProc) pump(stdout io.Reader) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		m, err := parseEngineLine(line)
		if err != nil {
			p.log.Printf("webpreview: %v", err)
			continue
		}
		switch m.Type {
		case "ready":
			select {
			case <-p.ready:
			default:
				close(p.ready)
			}
		case "partial":
			p.deliver(Result{Text: m.Text})
		case "final":
			p.deliver(Result{Text: m.Text, Final: true})
		case "error":
			p.deliver(Result{Final: true, Err: fmt.Errorf("voice engine: %s", m.Error)})
		}
	}
	if sc.Err() != nil {
		// Scan stopped on something other than clean EOF (e.g. a
		// too-long line) — the process itself may still be running.
		// Reaping it here is what actually frees its ~700 MB mmap;
		// without it, marking dead below just lets ensureProc spawn a
		// replacement while this one leaks, unreaped, in the background.
		// Can't reuse stop(): it treats a closed p.dead as "already
		// exited, nothing to kill", but this goroutine is about to close
		// p.dead itself regardless of whether the process actually died.
		// So: close stdin for a clean exit attempt (a scan error can land
		// while the process is still faulting in the model, and SIGKILL
		// during that mmap is what wedged macOS into uninterruptible exit
		// before), then kill unconditionally after grace.
		p.log.Printf("webpreview: clank-voice stdout scan error: %v", sc.Err())
		p.writeMu.Lock()
		_ = p.stdin.Close()
		p.writeMu.Unlock()
		go func() {
			time.Sleep(2 * time.Second)
			if p.cmd.Process != nil {
				_ = p.cmd.Process.Kill()
			}
		}()
	}
	// Stdout EOF = the process is gone. Mark it dead BEFORE notifying,
	// so a listener that reacts to the error by reopening can't be
	// handed the corpse by ensureProc.
	close(p.dead)
	p.deliver(Result{Final: true, Err: fmt.Errorf("voice engine process died")})
}

type sherpaSession struct {
	engine *SherpaEngine
	proc   *voiceProc
	ch     chan Result

	mu     sync.Mutex
	closed bool
}

func (s *sherpaSession) Feed(pcm []byte) error {
	// Chunk to the frame cap; the worklet sends ~4 KiB so this loop is
	// almost always a single iteration.
	for len(pcm) > 0 {
		n := len(pcm)
		if n > maxFramePayload {
			n = maxFramePayload
		}
		if err := s.proc.writeFrame(frameAudio, pcm[:n]); err != nil {
			return err
		}
		pcm = pcm[n:]
	}
	return nil
}

func (s *sherpaSession) End() error    { return s.proc.writeFrame(frameEnd, nil) }
func (s *sherpaSession) Cancel() error { return s.proc.writeFrame(frameCancel, nil) }

func (s *sherpaSession) Results() <-chan Result { return s.ch }

func (s *sherpaSession) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	// Discard any half-fed utterance so the next session starts clean,
	// then stop routing before closing the channel the pump writes to.
	_ = s.proc.writeFrame(frameCancel, nil)
	s.proc.detach()
	close(s.ch)
	<-s.engine.sem
	return nil
}

// logWriter adapts a *log.Logger for subprocess stderr.
type logWriter struct {
	log    *log.Logger
	prefix string
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.log.Printf("%s%s", w.prefix, string(p))
	return len(p), nil
}

// FindClankVoice locates the clank-voice binary: next to the running
// executable first (paired installs), then PATH — the same resolution
// clank uses for clankd and the daemon uses for clank-host.
func FindClankVoice() (string, error) {
	if self, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(self), "clank-voice")
		if info, serr := os.Stat(sibling); serr == nil && !info.IsDir() {
			return sibling, nil
		}
	}
	if found, err := exec.LookPath("clank-voice"); err == nil {
		return found, nil
	}
	return "", fmt.Errorf("clank-voice not installed (build it from voice-engine/ or set %s)", EngineEnvVar)
}
