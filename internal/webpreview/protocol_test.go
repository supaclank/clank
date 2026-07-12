package webpreview

import (
	"bytes"
	"io"
	"testing"
)

func TestFrameRoundtrip(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	payload := []byte{1, 2, 3, 4, 5}
	if err := writeFrame(&buf, frameAudio, payload); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	if err := writeFrame(&buf, frameEnd, nil); err != nil {
		t.Fatalf("writeFrame end: %v", err)
	}

	typ, got, err := readFrame(&buf)
	if err != nil || typ != frameAudio || !bytes.Equal(got, payload) {
		t.Fatalf("frame 1 = (%d, %v, %v), want (audio, payload, nil)", typ, got, err)
	}
	typ, got, err = readFrame(&buf)
	if err != nil || typ != frameEnd || len(got) != 0 {
		t.Fatalf("frame 2 = (%d, %v, %v), want (end, empty, nil)", typ, got, err)
	}
	if _, _, err := readFrame(&buf); err != io.EOF {
		t.Fatalf("empty stream: err = %v, want io.EOF", err)
	}
}

func TestFrameRejectsOversizedPayload(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := writeFrame(&buf, frameAudio, make([]byte, maxFramePayload+1)); err == nil {
		t.Fatalf("writeFrame: oversized payload must be rejected")
	}
	// A malicious/corrupt header claiming a huge payload must be
	// rejected before allocation.
	buf.Write([]byte{frameAudio, 0xff, 0xff, 0xff, 0xff})
	if _, _, err := readFrame(&buf); err == nil {
		t.Fatalf("readFrame: oversized header must be rejected")
	}
}

func TestParseEngineLine(t *testing.T) {
	t.Parallel()
	m, err := parseEngineLine([]byte(`{"type":"partial","text":"hello"}`))
	if err != nil || m.Type != "partial" || m.Text != "hello" {
		t.Fatalf("parseEngineLine = (%+v, %v)", m, err)
	}
	if _, err := parseEngineLine([]byte(`nonsense`)); err == nil {
		t.Fatalf("parseEngineLine: want error for non-JSON line")
	}
}
