package clankcli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPreviewLogDelta(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		previous string
		current  string
		want     string
	}{
		{name: "initial snapshot", current: "installing\n", want: "installing\n"},
		{name: "appended output", previous: "installing\n", current: "installing\nready\n", want: "ready\n"},
		{name: "unchanged", previous: "installing\n", current: "installing\n"},
		{name: "ring wrapped", previous: "abcdef", current: "defghi", want: "ghi"},
		{name: "ring advanced beyond overlap", previous: "abcdef", current: "uvwxyz", want: "uvwxyz"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := string(previewLogDelta([]byte(tt.previous), []byte(tt.current))); got != tt.want {
				t.Errorf("previewLogDelta(%q, %q) = %q, want %q", tt.previous, tt.current, got, tt.want)
			}
		})
	}
}

func TestPreviewStartupLogStreamApplyIgnoresFetchError(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	stream := &previewStartupLogStream{out: &out}
	if err := stream.apply(previewLogResult{err: errors.New("boom")}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("apply wrote output despite a fetch error:\n%s", out.String())
	}
}

// TestPreviewLogPollerCoalescesConcurrentPolls pins the concurrency contract
// review bots flagged on PR #261: a slow log fetch must not block whatever
// loop drives Poll, and only one fetch may be in flight at a time.
func TestPreviewLogPollerCoalescesConcurrentPolls(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	var calls int32
	fetch := func(ctx context.Context) ([]byte, error) {
		atomic.AddInt32(&calls, 1)
		<-release
		return []byte("logs"), nil
	}
	var out bytes.Buffer
	stream := &previewStartupLogStream{out: &out}
	poller := newPreviewLogPoller(stream, fetch)

	pollReturned := make(chan struct{})
	go func() {
		poller.Poll(context.Background())
		close(pollReturned)
	}()
	select {
	case <-pollReturned:
	case <-time.After(time.Second):
		t.Fatal("Poll blocked on a slow fetch instead of returning immediately")
	}

	// Two more calls while the fetch above is still in flight must not start
	// a second fetch.
	poller.Poll(context.Background())
	poller.Poll(context.Background())

	close(release)
	select {
	case res := <-poller.C:
		if res.err != nil {
			t.Fatalf("unexpected fetch error: %v", res.err)
		}
	case <-time.After(time.Second):
		t.Fatal("result never delivered on C")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("fetch invoked %d times while a poll was in flight, want 1", got)
	}
}

func TestPreviewLogPollerAwaitAppliesPendingResult(t *testing.T) {
	t.Parallel()
	fetch := func(ctx context.Context) ([]byte, error) {
		return []byte("hello\n"), nil
	}
	var out bytes.Buffer
	stream := &previewStartupLogStream{out: &out}
	poller := newPreviewLogPoller(stream, fetch)

	poller.Poll(context.Background())
	if err := poller.Await(); err != nil {
		t.Fatalf("Await: %v", err)
	}
	if !strings.Contains(out.String(), "hello") {
		t.Fatalf("Await did not apply the in-flight result:\n%s", out.String())
	}
	// No poll in flight now; Await must be a no-op rather than blocking.
	if err := poller.Await(); err != nil {
		t.Fatalf("second Await: %v", err)
	}
}
