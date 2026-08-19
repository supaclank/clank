package clankcli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	previewLogPollInterval = 1 * time.Second
	previewLogHeader       = "┌─ dev server output"
	previewLogGutter       = "│ "
	previewLogFooter       = "└─ end dev server output"
)

type previewStartupLogStream struct {
	out         io.Writer
	previous    []byte
	hasOutput   bool
	isLineStart bool
	lastWasCR   bool
	lastByte    byte
}

// previewLogFetcher reads the current dev-server log snapshot. It's a plain
// function (typically client.Logs) rather than *daemonclient.PreviewClient
// so previewLogPoller can be driven by a synthetic fetcher in tests.
type previewLogFetcher func(ctx context.Context) ([]byte, error)

type previewLogResult struct {
	logs []byte
	err  error
}

// fetch reads one log snapshot without touching stream state, so it's safe
// to run on a background goroutine alongside apply on the caller's own
// goroutine.
func (s *previewStartupLogStream) fetch(ctx context.Context, fetch previewLogFetcher) previewLogResult {
	readCtx, cancel := context.WithTimeout(ctx, previewLogReadTimeout)
	defer cancel()
	logs, err := fetch(readCtx)
	return previewLogResult{logs: logs, err: err}
}

// apply folds a fetched snapshot into the stream, writing any new output.
func (s *previewStartupLogStream) apply(res previewLogResult) error {
	if res.err != nil {
		// Progress is best-effort; readiness status remains the source of truth.
		return nil
	}
	delta := previewLogDelta(s.previous, res.logs)
	s.previous = res.logs
	if len(delta) == 0 {
		return nil
	}
	return s.write(delta)
}

func (s *previewStartupLogStream) write(chunk []byte) error {
	if len(chunk) == 0 {
		return nil
	}
	if !s.hasOutput {
		if _, err := fmt.Fprintln(s.out, "\n"+styleCmdHint.Render(previewLogHeader)); err != nil {
			return fmt.Errorf("write preview log heading: %w", err)
		}
		s.hasOutput = true
		s.isLineStart = true
	}
	framed := make([]byte, 0, len(chunk)+len(previewLogGutter))
	for _, b := range chunk {
		if s.isLineStart {
			if b == '\n' && s.lastWasCR {
				framed = append(framed, b)
				s.lastWasCR = false
				continue
			}
			framed = append(framed, previewLogGutter...)
			s.isLineStart = false
		}
		framed = append(framed, b)
		s.lastWasCR = b == '\r'
		if b == '\n' || b == '\r' {
			s.isLineStart = true
		}
	}
	if _, err := io.WriteString(s.out, renderPreviewLog(framed)); err != nil {
		return fmt.Errorf("write preview startup logs: %w", err)
	}
	s.lastByte = chunk[len(chunk)-1]
	return nil
}

// poll fetches and applies a log snapshot synchronously.
func (s *previewStartupLogStream) poll(ctx context.Context, fetch previewLogFetcher) error {
	return s.apply(s.fetch(ctx, fetch))
}

func renderPreviewLog(log []byte) string {
	var rendered strings.Builder
	lineStart := 0
	for i, b := range log {
		if b != '\n' && b != '\r' {
			continue
		}
		if lineStart < i {
			rendered.WriteString(stylePreviewLog.Render(string(log[lineStart:i])))
		}
		rendered.WriteByte(b)
		lineStart = i + 1
	}
	if lineStart < len(log) {
		rendered.WriteString(stylePreviewLog.Render(string(log[lineStart:])))
	}
	return rendered.String()
}

func (s *previewStartupLogStream) finish() {
	if !s.hasOutput {
		return
	}
	if s.lastByte != '\n' {
		_, _ = fmt.Fprintln(s.out)
	}
	_, _ = fmt.Fprintln(s.out, styleCmdHint.Render(previewLogFooter))
}

// previewLogPoller dispatches at most one in-flight log fetch at a time, so
// a slow read (up to previewLogReadTimeout) never blocks a select statement
// it shares with a readiness-status poll.
type previewLogPoller struct {
	fetch    previewLogFetcher
	stream   *previewStartupLogStream
	C        chan previewLogResult
	inFlight bool
}

func newPreviewLogPoller(stream *previewStartupLogStream, fetch previewLogFetcher) *previewLogPoller {
	return &previewLogPoller{fetch: fetch, stream: stream, C: make(chan previewLogResult, 1)}
}

// Poll starts a fetch in the background unless one is already running.
func (p *previewLogPoller) Poll(ctx context.Context) {
	if p.inFlight {
		return
	}
	p.inFlight = true
	go func() { p.C <- p.stream.fetch(ctx, p.fetch) }()
}

// Ack marks the most recently started fetch (received from C) as delivered,
// so the next Poll call can start a new one.
func (p *previewLogPoller) Ack() {
	p.inFlight = false
}

// Await blocks for the in-flight fetch, if any, applies it, and reports
// whether a fresh snapshot was drained. A fetch that errored transiently
// does not count as fresh — apply() treats fetch errors as best-effort and
// swallows them, so the caller must still be told to retry. Call this before
// a final synchronous poll so it never races the background fetch.
func (p *previewLogPoller) Await() (fresh bool, err error) {
	if !p.inFlight {
		return false, nil
	}
	p.inFlight = false
	res := <-p.C
	if err := p.stream.apply(res); err != nil {
		return false, err
	}
	return res.err == nil, nil
}

// Flush applies the in-flight fetch if there is one, or otherwise issues a
// synchronous poll so the stream reflects the latest available snapshot
// before the caller exits. Skips the fallback poll only when Await already
// delivered a fresh snapshot — a redundant synchronous poll in that case
// would just add another previewLogReadTimeout of latency for no new data,
// whereas skipping it after a failed in-flight fetch would silently forfeit
// the caller's last chance at a final snapshot.
func (p *previewLogPoller) Flush(ctx context.Context) error {
	fresh, err := p.Await()
	if err != nil {
		return err
	}
	if fresh {
		return nil
	}
	return p.stream.poll(ctx, p.fetch)
}

func previewLogDelta(previous, current []byte) []byte {
	if len(previous) == 0 {
		return current
	}
	if bytes.HasPrefix(current, previous) {
		return current[len(previous):]
	}
	// A stable prefix shared by both snapshots (e.g. a startup banner kept
	// outside the host's eviction window) was already emitted in an earlier
	// delta; exclude it before hunting for the ring's own sliding-window
	// overlap, or it swallows the whole prefix on every remaining poll.
	//
	// TODO(ai-review): a coincidental ring-content prefix match (not the
	// banner) would also get excluded here, silently dropping output —
	// same class of risk as previewLogOverlap's own repeated-byte caveat
	// below, and the real fix is the same one already deferred for it: a
	// daemon-provided cursor/offset instead of content-based inference.
	// https://github.com/supaclank/clank/pull/265#discussion_r3812350071
	prefixLen := commonPrefixLen(previous, current)
	overlap := prefixLen + previewLogOverlap(previous[prefixLen:], current[prefixLen:])
	return current[overlap:]
}

// commonPrefixLen returns the length of the longest shared prefix of a and b.
func commonPrefixLen(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}

// previewLogOverlap returns the longest suffix of previous that is also a
// prefix of current. This prevents duplicate output when the host ring wraps.
//
// TODO(ai-review): a repeated-byte rollover boundary can make this textual
// overlap longer than the bytes actually retained, silently dropping new
// output.
// https://github.com/supaclank/clank/pull/261#discussion_r3803916265
// https://github.com/supaclank/clank/pull/261#discussion_r3804004378
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
