package clankcli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	daemonclient "github.com/supaclank/clank/internal/daemonclient"
)

const (
	previewLogPollInterval = 1 * time.Second
	previewLogHeader       = "--- dev server output ---"
)

type previewStartupLogStream struct {
	out       io.Writer
	previous  []byte
	hasOutput bool
	lastByte  byte
}

func (s *previewStartupLogStream) poll(ctx context.Context, client *daemonclient.PreviewClient) error {
	readCtx, cancel := context.WithTimeout(ctx, previewLogReadTimeout)
	defer cancel()
	logs, err := client.Logs(readCtx)
	if err != nil {
		// Progress is best-effort; readiness status remains the source of truth.
		return nil
	}
	delta := previewLogDelta(s.previous, logs)
	s.previous = logs
	if len(delta) == 0 {
		return nil
	}
	if !s.hasOutput {
		if _, err := fmt.Fprintln(s.out, "\n"+previewLogHeader); err != nil {
			return fmt.Errorf("write preview log heading: %w", err)
		}
		s.hasOutput = true
	}
	if _, err := s.out.Write(delta); err != nil {
		return fmt.Errorf("write preview startup logs: %w", err)
	}
	s.lastByte = delta[len(delta)-1]
	return nil
}

func (s *previewStartupLogStream) finish() {
	if s.hasOutput && s.lastByte != '\n' {
		_, _ = fmt.Fprintln(s.out)
	}
}

func previewLogDelta(previous, current []byte) []byte {
	if len(previous) == 0 {
		return current
	}
	if bytes.HasPrefix(current, previous) {
		return current[len(previous):]
	}
	overlap := previewLogOverlap(previous, current)
	return current[overlap:]
}

// previewLogOverlap returns the longest suffix of previous that is also a
// prefix of current. This prevents duplicate output when the host ring wraps.
func previewLogOverlap(previous, current []byte) int {
	if len(current) == 0 {
		return 0
	}
	prefix := make([]int, len(current))
	for i, matched := 1, 0; i < len(current); i++ {
		for matched > 0 && current[i] != current[matched] {
			matched = prefix[matched-1]
		}
		if current[i] == current[matched] {
			matched++
		}
		prefix[i] = matched
	}
	matched := 0
	for i, b := range previous {
		for matched > 0 && b != current[matched] {
			matched = prefix[matched-1]
		}
		if b == current[matched] {
			matched++
		}
		if matched == len(current) && i < len(previous)-1 {
			matched = prefix[matched-1]
		}
	}
	return matched
}
