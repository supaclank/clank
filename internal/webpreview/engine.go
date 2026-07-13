// Package webpreview is the browser twin of the phone preview: an
// overlay-injecting reverse proxy that `clank preview` puts in front of
// a KindWeb dev server (see internal/host/preview), plus the dictation
// service the overlay's push-to-talk streams into.
//
// The proxy owns three concerns on one loopback origin so the injected
// overlay never fights CORS or mixed-origin auth:
//
//   - serve the overlay assets (embedded, /__clank/overlay.js)
//   - relay /__clank/api/* to the daemon's unix socket behind a
//     per-run bearer token (the web analog of preview_frontdoor.go's
//     LAN pairing token)
//   - rewrite proxied HTML to inject the overlay <script> + config
//
// Dictation runs in this process rather than the daemon on purpose:
// it lives exactly as long as `clank preview` does, and keeping it out
// of the daemon means no host API surface until a second client (TUI,
// mobile-web) wants to share it.
package webpreview

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// EngineEnvVar overrides the dictation engine with a shell command that
// reads a 16 kHz mono s16le WAV on stdin and prints the transcript to
// stdout, e.g.
//
//	CLANK_VOICE_ASR_CMD='whisper-cli -m ~/models/ggml-base.en.bin -nt -f -'
//
// One process per utterance, final-only (no partials). When unset, the
// sherpa-onnx engine is used if a clank-voice binary is installed (see
// sherpa.go); otherwise voice is off.
const EngineEnvVar = "CLANK_VOICE_ASR_CMD"

// Result is one transcription update for the current utterance.
// Partials are cumulative (the text so far, not a delta), matching how
// clank-mobile's recognizer commits VAD segments monotonically.
type Result struct {
	Text  string
	Final bool
	// Err reports a failed decode (Final implied). The session may
	// still accept the next utterance unless Results was closed.
	Err error
}

// Engine produces dictation sessions. Implementations may serialize
// sessions (the sherpa engine funnels everything through one model
// process); Open blocks until the engine is free or ctx is done.
type Engine interface {
	// Open starts a session. The caller owns it and must Close it.
	Open(ctx context.Context) (Session, error)
	// Describe returns a short human-readable engine label for the
	// preview banner and logs.
	Describe() string
}

// Session is one dictation conversation, typically bound to one
// overlay WebSocket. PCM is s16le, 16 kHz, mono — the same shape
// clank-mobile feeds sherpa-onnx.
type Session interface {
	// Feed accepts PCM as it arrives from the mic.
	Feed(pcm []byte) error
	// End marks push-to-talk release: flush and decode; exactly one
	// Final Result follows on Results (empty text = heard nothing).
	End() error
	// Cancel discards audio buffered since the last End.
	Cancel() error
	// Results streams partial and final updates. Closed when the
	// session is Closed or the engine dies.
	Results() <-chan Result
	// Close releases the session and, for serializing engines, hands
	// the engine to the next Open.
	Close() error
}

// EngineFromEnv returns the exec-command engine configured via
// EngineEnvVar, or nil when unset.
func EngineFromEnv() Engine {
	cmdline := strings.TrimSpace(os.Getenv(EngineEnvVar))
	if cmdline == "" {
		return nil
	}
	return &ExecEngine{Cmdline: cmdline}
}

// ExecEngine shells out to a user-configured command per utterance.
// WAV on stdin (not raw PCM) so off-the-shelf tools — whisper.cpp's
// whisper-cli, sox pipelines, a custom script — work without flags
// describing the sample format. Final-only: partials would mean
// re-decoding the whole utterance per chunk at O(n²) cost.
type ExecEngine struct {
	Cmdline string
}

func (e *ExecEngine) Describe() string {
	c := e.Cmdline
	if len(c) > 40 {
		c = c[:40] + "…"
	}
	return "exec: " + c
}

func (e *ExecEngine) Open(ctx context.Context) (Session, error) {
	sctx, cancel := context.WithCancel(ctx)
	return &execSession{
		engine: e,
		ctx:    sctx,
		cancel: cancel,
		ch:     make(chan Result, 4),
	}, nil
}

type execSession struct {
	engine *ExecEngine
	ctx    context.Context
	cancel context.CancelFunc
	ch     chan Result
	wg     sync.WaitGroup

	mu     sync.Mutex
	buf    []byte
	closed bool
}

func (s *execSession) Feed(pcm []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("session closed")
	}
	s.buf = append(s.buf, pcm...)
	return nil
}

func (s *execSession) End() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("session closed")
	}
	utterance := s.buf
	s.buf = nil
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if len(utterance) == 0 {
			s.deliver(Result{Final: true})
			return
		}
		text, err := s.engine.transcribe(s.ctx, utterance)
		s.deliver(Result{Text: text, Final: true, Err: err})
	}()
	return nil
}

func (s *execSession) Cancel() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = nil
	return nil
}

func (s *execSession) Results() <-chan Result { return s.ch }

func (s *execSession) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	s.cancel()  // kills any in-flight command via CommandContext
	s.wg.Wait() // senders are done → closing is safe
	close(s.ch)
	return nil
}

func (s *execSession) deliver(r Result) {
	select {
	case s.ch <- r:
	case <-s.ctx.Done():
	}
}

// transcribe runs one utterance through the configured command.
func (e *ExecEngine) transcribe(ctx context.Context, pcm []byte) (string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", e.Cmdline)
	cmd.Stdin = bytes.NewReader(wavFromPCM(pcm))
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(errBuf.String())
		if len(detail) > 300 {
			detail = detail[len(detail)-300:]
		}
		if detail != "" {
			return "", fmt.Errorf("asr command: %w: %s", err, detail)
		}
		return "", fmt.Errorf("asr command: %w", err)
	}
	return strings.TrimSpace(out.String()), nil
}

// Dictation audio constants — mirrored by overlay/worklet.js, which
// resamples the mic to this rate before streaming.
const (
	sampleRate    = 16000
	bytesPerFrame = 2 // s16le mono
)

// wavFromPCM wraps raw s16le/16kHz/mono PCM in a minimal RIFF header.
func wavFromPCM(pcm []byte) []byte {
	var b bytes.Buffer
	b.Grow(44 + len(pcm))
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+len(pcm)))
	b.WriteString("WAVEfmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))                       // fmt chunk size
	binary.Write(&b, binary.LittleEndian, uint16(1))                        // PCM
	binary.Write(&b, binary.LittleEndian, uint16(1))                        // mono
	binary.Write(&b, binary.LittleEndian, uint32(sampleRate))               // sample rate
	binary.Write(&b, binary.LittleEndian, uint32(sampleRate*bytesPerFrame)) // byte rate
	binary.Write(&b, binary.LittleEndian, uint16(bytesPerFrame))            // block align
	binary.Write(&b, binary.LittleEndian, uint16(8*bytesPerFrame))          // bits/sample
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(len(pcm)))
	b.Write(pcm)
	return b.Bytes()
}
