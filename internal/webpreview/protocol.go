package webpreview

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// Wire protocol between clank and the clank-voice subprocess.
//
// clank → clank-voice (stdin): binary frames
//
//	[1 byte type][4 bytes little-endian payload length][payload]
//
// clank-voice → clank (stdout): one JSON object per line
//
//	{"type":"ready"}
//	{"type":"partial","text":"..."}   cumulative text so far
//	{"type":"final","text":"..."}     exactly one per end frame
//	{"type":"error","error":"..."}
//
// voice-engine/cmd/clank-voice mirrors these definitions — the modules
// are deliberately independent (the voice-engine module carries the
// cgo sherpa-onnx dependency and must not be required by clank, and
// clank must not be required by it), so keep the two copies in sync by
// hand. It is five constants and two functions on purpose.
const (
	frameAudio  = byte(0) // payload: s16le 16kHz mono PCM
	frameEnd    = byte(1) // no payload: decode the utterance
	frameCancel = byte(2) // no payload: discard the utterance

	// maxFramePayload bounds one frame, not one utterance (the ws layer
	// caps utterances). The overlay worklet sends ~4 KiB per 128 ms.
	maxFramePayload = 1 << 20
)

// engineMsg is a clank-voice stdout line.
type engineMsg struct {
	Type  string `json:"type"`
	Text  string `json:"text,omitempty"`
	Error string `json:"error,omitempty"`
}

func writeFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > maxFramePayload {
		return fmt.Errorf("frame payload %d exceeds %d", len(payload), maxFramePayload)
	}
	var hdr [5]byte
	hdr[0] = typ
	binary.LittleEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

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

// parseEngineLine decodes one clank-voice stdout line.
func parseEngineLine(line []byte) (engineMsg, error) {
	var m engineMsg
	if err := json.Unmarshal(line, &m); err != nil {
		return engineMsg{}, fmt.Errorf("engine line %.80q: %w", line, err)
	}
	return m, nil
}
