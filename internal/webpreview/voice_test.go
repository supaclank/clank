package webpreview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// stubEngine scripts Results without any real transcription: every Feed
// emits a cumulative partial, End emits a final with the byte count.
type stubEngine struct {
	openErr error
}

func (e *stubEngine) Describe() string { return "stub" }

func (e *stubEngine) Open(ctx context.Context) (Session, error) {
	if e.openErr != nil {
		return nil, e.openErr
	}
	return &stubSession{ch: make(chan Result, 8)}, nil
}

type stubSession struct {
	mu     sync.Mutex
	fed    int
	closed bool
	ch     chan Result

	// feedErr/endErr, when set, make Feed/End fail without producing a
	// Result — simulating a broken pipe / dead subprocess, where the
	// pump's Err/Final delivery isn't what surfaces the failure.
	feedErr error
	endErr  error
}

func (s *stubSession) Feed(pcm []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.feedErr != nil {
		return s.feedErr
	}
	s.fed += len(pcm)
	s.ch <- Result{Text: fmt.Sprintf("got %d", s.fed)}
	return nil
}

func (s *stubSession) End() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.endErr != nil {
		return s.endErr
	}
	s.ch <- Result{Text: fmt.Sprintf("final %d", s.fed), Final: true}
	s.fed = 0
	return nil
}

func (s *stubSession) Cancel() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fed = 0
	return nil
}

func (s *stubSession) Results() <-chan Result { return s.ch }

func (s *stubSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.ch)
	}
	return nil
}

// testDeadlinePerStepCap bounds a genuine hang (not just a loaded CI
// runner) to a fraction of the full test timeout, so it fails in
// seconds instead of blocking until -timeout's ~10m default. It's set
// well above the ~10s stall this package's flake exhibited under load.
const testDeadlinePerStepCap = time.Minute

// capDeadline is testDeadline's logic without the *testing.T dependency,
// so it can be table-tested without varying the real -timeout flag.
func capDeadline(now, deadline time.Time, hasDeadline bool, perStepCap time.Duration) time.Time {
	capped := now.Add(perStepCap)
	if hasDeadline {
		if hard := deadline.Add(-5 * time.Second); hard.Before(capped) {
			return hard
		}
	}
	return capped
}

// testDeadline bounds any single blocking step (dial, read, slot poll)
// by the test binary's own -timeout deadline instead of a small fixed
// budget. Fixed budgets flake: a loaded CI runner can stall a "can't
// possibly take 10s" step for exactly that long. The grace keeps a
// failure inside the test, as a t.Fatalf with the step's context,
// rather than the framework's global-timeout panic.
func testDeadline(t *testing.T) time.Time {
	t.Helper()
	d, ok := t.Deadline()
	return capDeadline(time.Now(), d, ok, testDeadlinePerStepCap)
}

// dialVoice spins a voice ws endpoint around engine and connects.
func dialVoice(t *testing.T, engine Engine) (*websocket.Conn, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveVoiceWS(w, r, engine, log.New(io.Discard, "", 0))
	}))
	ctx, cancel := context.WithDeadline(context.Background(), testDeadline(t))
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		cancel()
		srv.Close()
		t.Fatalf("dial: %v", err)
	}
	return conn, func() {
		conn.Close(websocket.StatusNormalClosure, "")
		cancel()
		srv.Close()
	}
}

func readVoiceMsg(t *testing.T, conn *websocket.Conn) voiceMsg {
	t.Helper()
	ctx, cancel := context.WithDeadline(context.Background(), testDeadline(t))
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	var m voiceMsg
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("ws message %q: %v", data, err)
	}
	return m
}

func TestCapDeadline(t *testing.T) {
	t.Parallel()
	now := time.Now()
	const cap = time.Minute

	t.Run("no test deadline falls back to the cap", func(t *testing.T) {
		t.Parallel()
		got := capDeadline(now, time.Time{}, false, cap)
		if !got.Equal(now.Add(cap)) {
			t.Fatalf("got %v, want %v", got, now.Add(cap))
		}
	})

	t.Run("far test deadline is capped, not used directly", func(t *testing.T) {
		t.Parallel()
		far := now.Add(10 * time.Minute)
		got := capDeadline(now, far, true, cap)
		if !got.Equal(now.Add(cap)) {
			t.Fatalf("got %v, want the %v cap rather than the far deadline", got, cap)
		}
	})

	t.Run("near test deadline wins over the cap", func(t *testing.T) {
		t.Parallel()
		near := now.Add(10 * time.Second)
		got := capDeadline(now, near, true, cap)
		want := near.Add(-5 * time.Second)
		if !got.Equal(want) {
			t.Fatalf("got %v, want %v (deadline - 5s)", got, want)
		}
	})
}

func TestVoiceWSStreamsPartialsAndFinal(t *testing.T) {
	t.Parallel()
	conn, done := dialVoice(t, &stubEngine{})
	defer done()

	ctx := context.Background()
	if err := conn.Write(ctx, websocket.MessageBinary, make([]byte, 10)); err != nil {
		t.Fatalf("write pcm: %v", err)
	}
	if m := readVoiceMsg(t, conn); m.Type != "partial" || m.Text != "got 10" {
		t.Fatalf("first message = %+v, want partial 'got 10'", m)
	}

	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"end"}`)); err != nil {
		t.Fatalf("write end: %v", err)
	}
	// transcribing ack then the final; both must arrive, in that order.
	if m := readVoiceMsg(t, conn); m.Type != "transcribing" {
		t.Fatalf("after end = %+v, want transcribing", m)
	}
	if m := readVoiceMsg(t, conn); m.Type != "final" || m.Text != "final 10" {
		t.Fatalf("final = %+v, want 'final 10'", m)
	}
}

func TestVoiceWSReportsEngineBusy(t *testing.T) {
	t.Parallel()
	conn, done := dialVoice(t, &stubEngine{openErr: errors.New("model held by another tab")})
	defer done()

	// Sessions are acquired lazily on the first audio frame — connecting
	// alone must not touch (or report on) the engine.
	if err := conn.Write(context.Background(), websocket.MessageBinary, make([]byte, 4)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if m := readVoiceMsg(t, conn); m.Type != "error" || !strings.Contains(m.Error, "model held") {
		t.Fatalf("busy engine message = %+v, want error mentioning the cause", m)
	}
}

// countingBusyEngine always fails Open and counts calls, so a test can
// assert the read loop stops retrying a busy engine on every audio frame.
type countingBusyEngine struct {
	opens int32
}

func (e *countingBusyEngine) Describe() string { return "counting-busy" }

func (e *countingBusyEngine) Open(ctx context.Context) (Session, error) {
	atomic.AddInt32(&e.opens, 1)
	return nil, errors.New("busy")
}

// TestVoiceWSOpenFailureSuppressesRepeatedOpens pins the fix for a stall:
// once an utterance's engine.Open fails, every subsequent audio frame in
// the same utterance used to retry Open (up to its own 10s timeout) on
// the read loop's goroutine, blocking cancel/end processing behind it.
func TestVoiceWSOpenFailureSuppressesRepeatedOpens(t *testing.T) {
	t.Parallel()
	eng := &countingBusyEngine{}
	conn, done := dialVoice(t, eng)
	defer done()

	ctx := context.Background()
	if err := conn.Write(ctx, websocket.MessageBinary, make([]byte, 4)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if m := readVoiceMsg(t, conn); m.Type != "error" {
		t.Fatalf("first frame = %+v, want error", m)
	}

	// A second frame in the same (still-failed) utterance must not
	// re-trigger Open.
	if err := conn.Write(ctx, websocket.MessageBinary, make([]byte, 4)); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"end"}`)); err != nil {
		t.Fatalf("write end: %v", err)
	}
	if m := readVoiceMsg(t, conn); m.Type != "transcribing" {
		t.Fatalf("after end = %+v, want transcribing", m)
	}

	// end clears the failure, so the next utterance retries Open. If a
	// spurious "final" had been queued for the failed utterance, it would
	// arrive here instead of the fresh "error" from the retry.
	if err := conn.Write(ctx, websocket.MessageBinary, make([]byte, 4)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if m := readVoiceMsg(t, conn); m.Type != "error" {
		t.Fatalf("next utterance frame = %+v, want error (not a spurious final)", m)
	}

	if got := atomic.LoadInt32(&eng.opens); got != 2 {
		t.Fatalf("Open called %d times, want exactly 2 (one per utterance)", got)
	}
}

// serialStub wraps stubEngine with a capacity-1 slot like SherpaEngine,
// erroring immediately when held — so leaks are loud in tests.
type serialStub struct {
	stubEngine
	slot chan struct{}

	// feedErr/endErr are threaded into every session this engine opens.
	feedErr error
	endErr  error
}

func newSerialStub() *serialStub {
	return &serialStub{slot: make(chan struct{}, 1)}
}

func (e *serialStub) Open(ctx context.Context) (Session, error) {
	select {
	case e.slot <- struct{}{}:
	default:
		return nil, errors.New("slot held")
	}
	return &serialSession{
		stubSession: stubSession{ch: make(chan Result, 8), feedErr: e.feedErr, endErr: e.endErr},
		release:     func() { <-e.slot },
	}, nil
}

type serialSession struct {
	stubSession
	release func()
	once    sync.Once
}

func (s *serialSession) Close() error {
	err := s.stubSession.Close()
	s.once.Do(s.release)
	return err
}

// An idle connection must not hold the engine slot: after one utterance
// completes, a SECOND connection dictates fine while the first stays
// open. Regression test for the zombie-socket lockout (a dead-but-
// pong-ing client used to hold the serialized engine forever).
func TestVoiceWSReleasesSlotBetweenUtterances(t *testing.T) {
	t.Parallel()
	eng := newSerialStub()
	conn1, done1 := dialVoice(t, eng)
	defer done1()

	ctx := context.Background()
	_ = conn1.Write(ctx, websocket.MessageBinary, make([]byte, 8))
	if m := readVoiceMsg(t, conn1); m.Type != "partial" {
		t.Fatalf("conn1 partial = %+v", m)
	}
	_ = conn1.Write(ctx, websocket.MessageText, []byte(`{"type":"end"}`))
	if m := readVoiceMsg(t, conn1); m.Type != "transcribing" {
		t.Fatalf("conn1 = %+v, want transcribing", m)
	}
	if m := readVoiceMsg(t, conn1); m.Type != "final" {
		t.Fatalf("conn1 = %+v, want final", m)
	}

	// conn1 stays connected (idle). conn2 must be able to dictate; the
	// slot release is async after the final, so poll.
	conn2, done2 := dialVoice(t, eng)
	defer done2()
	waitForSlot(t, conn2)
	_ = conn2.Write(ctx, websocket.MessageText, []byte(`{"type":"end"}`))
	got := []string{}
	for len(got) < 2 {
		got = append(got, readVoiceMsg(t, conn2).Type)
	}
	if got[len(got)-1] != "final" {
		t.Fatalf("conn2 sequence = %v, want …final", got)
	}
}

// waitForSlot polls conn by writing audio until the engine slot is
// acquired ("partial" comes back instead of "error"). The slot is
// released asynchronously by the other connection (cancel has no ack;
// the results pump writes the final before it Closes the session), so
// early frames can lose the race and draw an "error". Every retry must
// then start a fresh utterance with a cancel: one failed Open latches
// openFailed, after which the handler drops the utterance's remaining
// audio frames without replying (see openSess), so a same-utterance
// retry would block on a reply that never comes until the read deadline.
func waitForSlot(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	ctx := context.Background()
	deadline := testDeadline(t)
	for {
		_ = conn.Write(ctx, websocket.MessageBinary, make([]byte, 4))
		m := readVoiceMsg(t, conn)
		if m.Type == "partial" {
			return
		}
		if m.Type != "error" || time.Now().After(deadline) {
			t.Fatalf("slot never acquired, last message = %+v", m)
		}
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"cancel"}`))
		time.Sleep(50 * time.Millisecond)
	}
}

// A session that only Cancels (instead of Closing) on the client's
// {"type":"cancel"} leaves the engine's exclusive slot held for the rest
// of the connection's lifetime — bricking dictation for every other
// connection until this one disconnects, the exact zombie-lockout the
// per-utterance design (see serveVoiceWS's doc comment) exists to avoid.
func TestVoiceWSCancelReleasesSlot(t *testing.T) {
	t.Parallel()
	eng := newSerialStub()
	conn1, done1 := dialVoice(t, eng)
	defer done1()

	ctx := context.Background()
	_ = conn1.Write(ctx, websocket.MessageBinary, make([]byte, 8))
	if m := readVoiceMsg(t, conn1); m.Type != "partial" {
		t.Fatalf("conn1 partial = %+v", m)
	}
	_ = conn1.Write(ctx, websocket.MessageText, []byte(`{"type":"cancel"}`))

	// conn1 stays connected (idle after cancel); conn2 must still be able
	// to dictate.
	conn2, done2 := dialVoice(t, eng)
	defer done2()
	waitForSlot(t, conn2)
}

// Same lockout risk on the utterance-too-long path: erroring out on an
// oversized utterance must free the slot, not just discard the buffer.
func TestVoiceWSUtteranceTooLongReleasesSlot(t *testing.T) {
	t.Parallel()
	eng := newSerialStub()
	conn1, done1 := dialVoice(t, eng)
	defer done1()

	ctx := context.Background()
	const chunk = 1_000_000 // > maxUtteranceBytes after 17 chunks
	sent := 0
	for sent+chunk <= maxUtteranceBytes {
		if err := conn1.Write(ctx, websocket.MessageBinary, make([]byte, chunk)); err != nil {
			t.Fatalf("write: %v", err)
		}
		if m := readVoiceMsg(t, conn1); m.Type != "partial" {
			t.Fatalf("partial = %+v", m)
		}
		sent += chunk
	}
	if err := conn1.Write(ctx, websocket.MessageBinary, make([]byte, chunk)); err != nil {
		t.Fatalf("write overflow chunk: %v", err)
	}
	if m := readVoiceMsg(t, conn1); m.Type != "error" || !strings.Contains(m.Error, "too long") {
		t.Fatalf("overflow message = %+v, want a 'too long' error", m)
	}

	conn2, done2 := dialVoice(t, eng)
	defer done2()
	waitForSlot(t, conn2)
}

// A client that keeps sending after an oversized utterance must not be
// able to reopen a fresh session on the next frame and bypass the limit
// indefinitely — the connection itself has to end.
func TestVoiceWSUtteranceTooLongClosesConnection(t *testing.T) {
	t.Parallel()
	eng := newSerialStub()
	conn, done := dialVoice(t, eng)
	defer done()

	ctx := context.Background()
	const chunk = 1_000_000 // > maxUtteranceBytes after 17 chunks
	sent := 0
	for sent+chunk <= maxUtteranceBytes {
		if err := conn.Write(ctx, websocket.MessageBinary, make([]byte, chunk)); err != nil {
			t.Fatalf("write: %v", err)
		}
		if m := readVoiceMsg(t, conn); m.Type != "partial" {
			t.Fatalf("partial = %+v", m)
		}
		sent += chunk
	}
	if err := conn.Write(ctx, websocket.MessageBinary, make([]byte, chunk)); err != nil {
		t.Fatalf("write overflow chunk: %v", err)
	}
	if m := readVoiceMsg(t, conn); m.Type != "error" || !strings.Contains(m.Error, "too long") {
		t.Fatalf("overflow message = %+v, want a 'too long' error", m)
	}

	// The server must actively close the connection (a close frame, not
	// just any read error): with the read budget now the whole test's, a
	// bare err != nil check would let a deadline expiry pass vacuously.
	rctx, cancel := context.WithDeadline(context.Background(), testDeadline(t))
	defer cancel()
	if _, _, err := conn.Read(rctx); websocket.CloseStatus(err) == -1 {
		t.Fatalf("conn.Read = %v, want the server to close the connection", err)
	}
}

func TestVoiceWSCancelResetsUtterance(t *testing.T) {
	t.Parallel()
	conn, done := dialVoice(t, &stubEngine{})
	defer done()

	ctx := context.Background()
	_ = conn.Write(ctx, websocket.MessageBinary, make([]byte, 4))
	if m := readVoiceMsg(t, conn); m.Text != "got 4" {
		t.Fatalf("partial = %+v", m)
	}
	_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"cancel"}`))
	_ = conn.Write(ctx, websocket.MessageBinary, make([]byte, 6))
	if m := readVoiceMsg(t, conn); m.Text != "got 6" {
		t.Fatalf("post-cancel partial = %+v, want counter reset ('got 6')", m)
	}
}

// A broken pipe / dead subprocess makes Feed return an error directly,
// without ever producing an Err Result for the pump to react to. That
// must still be treated as terminal for the utterance — otherwise the
// session (and its exclusive engine slot) sits stranded until the
// connection itself closes.
func TestVoiceWSFeedErrorReleasesSlot(t *testing.T) {
	t.Parallel()
	eng := newSerialStub()
	eng.feedErr = errors.New("broken pipe")
	conn1, done1 := dialVoice(t, eng)
	defer done1()

	ctx := context.Background()
	if err := conn1.Write(ctx, websocket.MessageBinary, make([]byte, 4)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if m := readVoiceMsg(t, conn1); m.Type != "error" || !strings.Contains(m.Error, "broken pipe") {
		t.Fatalf("feed error message = %+v, want an error mentioning the cause", m)
	}
	eng.feedErr = nil // a real dead subprocess respawns healthy for the next session

	conn2, done2 := dialVoice(t, eng)
	defer done2()
	waitForSlot(t, conn2)
}

// Same lockout risk when End itself fails (vs. the pump seeing an Err
// Result): the session must still close so the slot is released.
func TestVoiceWSEndErrorReleasesSlot(t *testing.T) {
	t.Parallel()
	eng := newSerialStub()
	eng.endErr = errors.New("broken pipe")
	conn1, done1 := dialVoice(t, eng)
	defer done1()

	ctx := context.Background()
	_ = conn1.Write(ctx, websocket.MessageBinary, make([]byte, 4))
	if m := readVoiceMsg(t, conn1); m.Type != "partial" {
		t.Fatalf("partial = %+v", m)
	}
	_ = conn1.Write(ctx, websocket.MessageText, []byte(`{"type":"end"}`))
	if m := readVoiceMsg(t, conn1); m.Type != "transcribing" {
		t.Fatalf("after end = %+v, want transcribing", m)
	}
	if m := readVoiceMsg(t, conn1); m.Type != "error" || !strings.Contains(m.Error, "broken pipe") {
		t.Fatalf("end error message = %+v, want an error mentioning the cause", m)
	}
	eng.endErr = nil // a real dead subprocess respawns healthy for the next session

	conn2, done2 := dialVoice(t, eng)
	defer done2()
	waitForSlot(t, conn2)
}
