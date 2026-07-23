package host

import (
	"errors"
	"sync"
	"testing"
)

// Regression test for the floorOnce bug Greptile flagged on PR #185: a
// sync.Once-based probe caches a failure forever, so one transient
// error (e.g. a canceled ctx on the very first Prepare call) would
// have permanently broken the opencode ACP backend for the rest of
// the clank-host process.
func TestOnceUntilSuccess_RetriesAfterFailure(t *testing.T) {
	t.Parallel()

	var o onceUntilSuccess
	calls := 0
	failFirst := func() error {
		calls++
		if calls == 1 {
			return errors.New("transient")
		}
		return nil
	}

	if err := o.do(failFirst); err == nil {
		t.Fatalf("first call: want error, got nil")
	}
	if err := o.do(failFirst); err != nil {
		t.Fatalf("second call: want success, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (retried after failure)", calls)
	}

	// Once succeeded, fn must never run again.
	if err := o.do(func() error {
		t.Fatal("fn invoked after success — should have short-circuited")
		return nil
	}); err != nil {
		t.Fatalf("third call: want cached success, got %v", err)
	}
}

func TestOnceUntilSuccess_ConcurrentCallersSerialize(t *testing.T) {
	t.Parallel()

	var o onceUntilSuccess
	var calls int
	var mu sync.Mutex
	var wg sync.WaitGroup
	const n = 20
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_ = o.do(func() error {
				mu.Lock()
				calls++
				mu.Unlock()
				return nil
			})
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("calls = %d, want exactly 1 across %d concurrent callers", calls, n)
	}
}
