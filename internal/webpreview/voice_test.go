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
}

func (s *stubSession) Feed(pcm []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fed += len(pcm)
	s.ch <- Result{Text: fmt.Sprintf("got %d", s.fed)}
	return nil
}

func (s *stubSession) End() error {
	s.mu.Lock()
	defer s.mu.Unlock()
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

// dialVoice spins a voice ws endpoint around engine and connects.
func dialVoice(t *testing.T, engine Engine) (*websocket.Conn, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveVoiceWS(w, r, engine, log.New(io.Discard, "", 0))
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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

// serialStub wraps stubEngine with a capacity-1 slot like SherpaEngine,
// erroring immediately when held — so leaks are loud in tests.
type serialStub struct {
	stubEngine
	slot chan struct{}
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
	return &serialSession{stubSession: stubSession{ch: make(chan Result, 8)}, release: func() { <-e.slot }}, nil
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
	// slot release is async after the final, so poll briefly.
	conn2, done2 := dialVoice(t, eng)
	defer done2()
	deadline := time.Now().Add(3 * time.Second)
	for {
		_ = conn2.Write(ctx, websocket.MessageBinary, make([]byte, 4))
		m := readVoiceMsg(t, conn2)
		if m.Type == "partial" {
			break // got the slot
		}
		if m.Type != "error" || time.Now().After(deadline) {
			t.Fatalf("conn2 never acquired the slot, last = %+v", m)
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = conn2.Write(ctx, websocket.MessageText, []byte(`{"type":"end"}`))
	got := []string{}
	for len(got) < 2 {
		got = append(got, readVoiceMsg(t, conn2).Type)
	}
	if got[len(got)-1] != "final" {
		t.Fatalf("conn2 sequence = %v, want …final", got)
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
