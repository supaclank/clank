// Command clank-voice is clank's local dictation engine: sherpa-onnx
// with Silero VAD segmentation + Parakeet transducer ASR, mirroring
// clank-mobile's on-device recognizer (modules/voice-input,
// SpeechRecognizer.kt) — same models, same 16 kHz mono s16le audio,
// same VAD parameters, same per-segment offline decode that yields
// monotonic partial transcripts at bounded cost (each committed VAD
// segment is decoded exactly once; no O(n²) re-decode of the utterance).
//
// It is a long-lived subprocess: the ~670 MB model set loads once at
// startup, then utterances stream in over stdin and transcripts stream
// out as JSON lines. The framing is defined (and mirrored — the modules
// are deliberately independent) in clank's internal/webpreview/protocol.go:
//
//	stdin:  [1-byte type][4-byte LE length][payload]
//	        type 0 = PCM (s16le/16kHz/mono), 1 = end, 2 = cancel
//	stdout: {"type":"ready"} once models are loaded,
//	        {"type":"partial","text":...} cumulative text mid-utterance,
//	        {"type":"final","text":...} exactly one per end frame,
//	        {"type":"error","error":...}
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
)

const (
	sampleRate = 16000
	// vadWindow is Silero's required analysis window at 16 kHz.
	vadWindow = 512

	frameAudio  = byte(0)
	frameEnd    = byte(1)
	frameCancel = byte(2)

	maxFramePayload = 1 << 20

	// maxRawSamples bounds the raw-utterance fallback buffer (10 min).
	maxRawSamples = 10 * 60 * sampleRate
)

func main() {
	models := flag.String("models", "", "directory containing the parakeet + silero_vad model files")
	minSilence := flag.Float64("min-silence", 0.35, "VAD min trailing silence (s) to close a segment (mobile parity)")
	minSpeech := flag.Float64("min-speech", 0.25, "VAD min speech duration (s) for a segment to count")
	threads := flag.Int("threads", 2, "ASR decode threads (mobile parity)")
	debug := flag.Bool("debug", false, "log VAD segment boundaries and decode timings to stderr")
	flag.Parse()

	if err := run(*models, float32(*minSilence), float32(*minSpeech), *threads, *debug); err != nil {
		fmt.Fprintln(os.Stderr, "clank-voice:", err)
		os.Exit(1)
	}
}

func run(modelsDir string, minSilence, minSpeech float32, threads int, debug bool) error {
	if modelsDir == "" {
		return fmt.Errorf("--models is required")
	}
	for _, f := range []string{"encoder.int8.onnx", "decoder.int8.onnx", "joiner.int8.onnx", "tokens.txt", "silero_vad.onnx"} {
		if info, err := os.Stat(filepath.Join(modelsDir, f)); err != nil || info.Size() == 0 {
			return fmt.Errorf("model file %s missing from %s (clank preview downloads these)", f, modelsDir)
		}
	}

	out := &emitter{w: bufio.NewWriter(os.Stdout)}

	// Recognizer config mirrors SpeechRecognizer.kt: nemo_transducer,
	// featureDim 80, cpu provider.
	recCfg := sherpa.OfflineRecognizerConfig{
		FeatConfig: sherpa.FeatureConfig{SampleRate: sampleRate, FeatureDim: 80},
		ModelConfig: sherpa.OfflineModelConfig{
			Transducer: sherpa.OfflineTransducerModelConfig{
				Encoder: filepath.Join(modelsDir, "encoder.int8.onnx"),
				Decoder: filepath.Join(modelsDir, "decoder.int8.onnx"),
				Joiner:  filepath.Join(modelsDir, "joiner.int8.onnx"),
			},
			Tokens:     filepath.Join(modelsDir, "tokens.txt"),
			NumThreads: threads,
			Provider:   "cpu",
			ModelType:  "nemo_transducer",
		},
		DecodingMethod: "greedy_search",
	}
	rec := sherpa.NewOfflineRecognizer(&recCfg)
	if rec == nil {
		return fmt.Errorf("failed to create recognizer (bad model files?)")
	}
	defer sherpa.DeleteOfflineRecognizer(rec)

	vadCfg := sherpa.VadModelConfig{
		SileroVad: sherpa.SileroVadModelConfig{
			Model:              filepath.Join(modelsDir, "silero_vad.onnx"),
			Threshold:          0.5,
			MinSilenceDuration: minSilence,
			MinSpeechDuration:  minSpeech,
			WindowSize:         vadWindow,
		},
		SampleRate: sampleRate,
		NumThreads: 1,
		Provider:   "cpu",
	}
	// 60 s of internal buffer: segments are popped continuously, so this
	// only needs to cover one segment plus slack, not the utterance.
	vad := sherpa.NewVoiceActivityDetector(&vadCfg, 60)
	if vad == nil {
		return fmt.Errorf("failed to create VAD (bad silero_vad.onnx?)")
	}
	defer sherpa.DeleteVoiceActivityDetector(vad)

	p := &pipeline{rec: rec, vad: vad, out: out, debug: debug}
	out.emit(msg{Type: "ready"})

	in := bufio.NewReaderSize(os.Stdin, 64*1024)
	for {
		typ, payload, err := readFrame(in)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil // parent went away — normal shutdown
		}
		if err != nil {
			return fmt.Errorf("read frame: %w", err)
		}
		switch typ {
		case frameAudio:
			p.feed(payload)
		case frameEnd:
			p.end()
		case frameCancel:
			p.cancel()
		}
	}
}

// pipeline is the per-utterance VAD → ASR state machine.
type pipeline struct {
	rec   *sherpa.OfflineRecognizer
	vad   *sherpa.VoiceActivityDetector
	out   *emitter
	debug bool

	pending   []float32 // buffered until a full VAD window is available
	raw       []float32 // whole utterance, for the VAD-miss fallback
	committed []string  // decoded text of completed VAD segments
}

func (p *pipeline) debugf(format string, args ...any) {
	if p.debug {
		fmt.Fprintf(os.Stderr, "clank-voice: "+format+"\n", args...)
	}
}

func (p *pipeline) feed(pcm []byte) {
	samples := s16leToFloat32(pcm)
	if len(p.raw) < maxRawSamples {
		room := maxRawSamples - len(p.raw)
		if len(samples) < room {
			room = len(samples)
		}
		p.raw = append(p.raw, samples[:room]...)
	}
	p.pending = append(p.pending, samples...)
	for len(p.pending) >= vadWindow {
		p.vad.AcceptWaveform(p.pending[:vadWindow])
		p.pending = p.pending[vadWindow:]
	}
	p.drain(true)
}

// drain decodes every completed VAD segment exactly once and, when
// emitPartials is set, publishes the cumulative committed text.
func (p *pipeline) drain(emitPartials bool) {
	for !p.vad.IsEmpty() {
		seg := p.vad.Front()
		p.vad.Pop()
		if seg == nil || len(seg.Samples) == 0 {
			continue
		}
		p.debugf("segment: start=%d samples=%d (%.2fs)", seg.Start, len(seg.Samples), float64(len(seg.Samples))/sampleRate)
		if text := p.decode(seg.Samples); text != "" {
			p.debugf("segment text: %q", text)
			p.committed = append(p.committed, text)
			if emitPartials {
				p.out.emit(msg{Type: "partial", Text: strings.Join(p.committed, " ")})
			}
		}
	}
}

func (p *pipeline) end() {
	// Flush forces the in-progress segment out of the VAD even without
	// trailing silence — push-to-talk release IS the boundary.
	p.vad.Flush()
	p.drain(false)
	final := strings.Join(p.committed, " ")
	if final == "" && len(p.raw) >= sampleRate/2 {
		// VAD heard no speech but there is real audio — decode it raw.
		// Short PTT bursts ("make it blue") can end before Silero's
		// min-speech window is satisfied.
		p.debugf("VAD committed nothing; raw fallback over %d samples", len(p.raw))
		final = p.decode(p.raw)
	}
	p.out.emit(msg{Type: "final", Text: final})
	p.reset()
}

func (p *pipeline) cancel() {
	p.vad.Clear()
	p.reset()
}

// reset restores per-utterance state. Flush marks end-of-stream inside
// the VAD, so Reset is mandatory before the next utterance — without it
// the detector misbehaves on everything after the first push-to-talk.
func (p *pipeline) reset() {
	p.vad.Reset()
	p.pending = p.pending[:0]
	p.raw = p.raw[:0]
	p.committed = nil
}

func (p *pipeline) decode(samples []float32) string {
	stream := sherpa.NewOfflineStream(p.rec)
	defer sherpa.DeleteOfflineStream(stream)
	stream.AcceptWaveform(sampleRate, samples)
	p.rec.Decode(stream)
	result := stream.GetResult()
	if result == nil {
		return ""
	}
	return strings.TrimSpace(result.Text)
}

func s16leToFloat32(pcm []byte) []float32 {
	n := len(pcm) / 2
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		out[i] = float32(int16(binary.LittleEndian.Uint16(pcm[2*i:]))) / 32768.0
	}
	return out
}

// msg is one stdout line. Mirror of internal/webpreview's engineMsg.
type msg struct {
	Type  string `json:"type"`
	Text  string `json:"text,omitempty"`
	Error string `json:"error,omitempty"`
}

type emitter struct {
	w *bufio.Writer
}

func (e *emitter) emit(m msg) {
	data, _ := json.Marshal(m)
	e.w.Write(data)
	e.w.WriteByte('\n')
	e.w.Flush() // line-per-message; the parent reads with a line scanner
}

// readFrame mirrors internal/webpreview/protocol.go.
func readFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.LittleEndian.Uint32(hdr[1:])
	if n > maxFramePayload {
		return 0, nil, fmt.Errorf("frame payload %d exceeds %d", n, maxFramePayload)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return hdr[0], payload, nil
}
