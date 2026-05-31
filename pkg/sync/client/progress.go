package syncclient

import (
	"io"
	"os"
)

// Push phase labels emitted via PushObserver.Phase.
const (
	PhaseBuilding  = "Building bundle"
	PhaseUploading = "Uploading"
)

// PushObserver receives progress events during PushCheckpoint so callers
// can render a live indicator (size, bytes uploaded, current phase). All
// methods may be called from the upload path; the CLI implementation
// forwards them to a goroutine-safe tea.Program. A nil observer disables
// reporting entirely — the autopush-hook (non-TTY) path passes nil.
type PushObserver interface {
	// Phase announces a named stage, e.g. PhaseBuilding / PhaseUploading.
	Phase(name string)
	// UploadSized reports the total bytes about to be uploaded (head +
	// uncommitted + manifest), known once the bundles are built.
	UploadSized(totalBytes int64)
	// UploadProgress reports cumulative bytes uploaded so far.
	UploadProgress(uploadedBytes int64)
}

// countingReader reports bytes read via onAdvance (a per-Read delta), used
// to drive upload progress without buffering the body. onAdvance must not
// block; the CLI forwards to a non-blocking tea.Program.Send.
type countingReader struct {
	r         io.Reader
	onAdvance func(int64)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 && c.onAdvance != nil {
		c.onAdvance(int64(n))
	}
	return n, err
}

// reportPhase is a nil-safe Phase call.
func reportPhase(obs PushObserver, name string) {
	if obs != nil {
		obs.Phase(name)
	}
}

// fileSize returns path's size, or 0 if empty/unstatable — the total is a
// best-effort progress hint, not a correctness input.
func fileSize(path string) int64 {
	if path == "" {
		return 0
	}
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}
