package bridge

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually-advanced clock so ceremony timing (window
// lease, attempt TTL, lockout) is deterministic.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Unix(1_700_000_000, 0)} }
func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newPairing(t *testing.T) (*Pairing, *Store, *fakeClock) {
	t.Helper()
	s, _ := testStore(t)
	clk := newClock()
	return NewPairing(s, clk.now), s, clk
}

func TestPairingHappyPath(t *testing.T) {
	t.Parallel()
	p, store, _ := newPairing(t)
	p.RefreshWindow() // a CLI is showing the QR

	id, code, err := p.Begin("Pixel 8")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if len(code) != pairCodeDigits {
		t.Fatalf("code %q not %d digits", code, pairCodeDigits)
	}

	// Before approval, the phone's poll sees pending, no secret.
	if state, secret := p.PollAttempt(id); state != AttemptPending || secret != nil {
		t.Fatalf("pre-approval poll = %s/%v, want pending/nil", state, secret)
	}

	// The laptop user types the code — separators tolerated.
	device, err := p.Complete("  " + code[:3] + " " + code[3:] + " ")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if device != "Pixel 8" {
		t.Fatalf("device = %q, want Pixel 8", device)
	}

	// First post-approval poll delivers the root secret exactly once.
	state, secret := p.PollAttempt(id)
	if state != AttemptApproved || !bytes.Equal(secret, store.Root()) {
		t.Fatalf("delivery poll = %s/%x, want approved + root", state, secret)
	}
	if _, again := p.PollAttempt(id); again != nil {
		t.Fatalf("secret delivered twice: %x", again)
	}
}

func TestPairingWindowGating(t *testing.T) {
	t.Parallel()
	p, _, clk := newPairing(t)

	// No window yet → Begin refused.
	if _, _, err := p.Begin("phone"); err != ErrPairWindowClosed {
		t.Fatalf("Begin with no window = %v, want ErrPairWindowClosed", err)
	}

	p.RefreshWindow()
	if _, _, err := p.Begin("phone"); err != nil {
		t.Fatalf("Begin in window: %v", err)
	}

	// Lease expires without a refresh → window closes.
	clk.advance(pairWindowLease + time.Second)
	if _, _, err := p.Begin("phone2"); err != ErrPairWindowClosed {
		t.Fatalf("Begin after lease = %v, want ErrPairWindowClosed", err)
	}
}

// TestPairingTypedCodeSelectsAttempt pins the core rule: with two
// phones waiting, the typed code approves the one that holds it —
// device names never gate approval.
func TestPairingTypedCodeSelectsAttempt(t *testing.T) {
	t.Parallel()
	p, _, _ := newPairing(t)
	p.RefreshWindow()

	idA, codeA, _ := p.Begin("Attacker Device")
	idB, codeB, _ := p.Begin("My Pixel")
	if codeA == codeB {
		t.Fatal("distinct attempts drew identical codes")
	}

	// Type MY phone's code — only my attempt approves.
	device, err := p.Complete(codeB)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if device != "My Pixel" {
		t.Fatalf("approved %q, want My Pixel", device)
	}
	if state, _ := p.PollAttempt(idB); state != AttemptApproved {
		t.Fatal("my attempt should be approved")
	}
	if state, secret := p.PollAttempt(idA); state != AttemptPending || secret != nil {
		t.Fatalf("attacker attempt = %s/%v, want still pending/nil", state, secret)
	}
}

func TestPairingPendingCap(t *testing.T) {
	t.Parallel()
	p, _, _ := newPairing(t)
	p.RefreshWindow()
	for i := 0; i < pairMaxPending; i++ {
		if _, _, err := p.Begin("phone"); err != nil {
			t.Fatalf("Begin %d: %v", i, err)
		}
	}
	if _, _, err := p.Begin("one too many"); err != ErrPairTooManyPending {
		t.Fatalf("over-cap Begin = %v, want ErrPairTooManyPending", err)
	}
}

func TestPairingLockout(t *testing.T) {
	t.Parallel()
	p, _, clk := newPairing(t)
	p.RefreshWindow()
	p.Begin("phone")

	for i := 0; i < pairMaxWrong; i++ {
		if _, err := p.Complete("000000"); err != ErrPairCodeMismatch {
			t.Fatalf("wrong %d = %v, want mismatch", i, err)
		}
		p.RefreshWindow() // per-second poll must NOT reset the counter
	}
	if _, err := p.Complete("000000"); err != ErrPairLockedOut {
		t.Fatalf("post-lockout = %v, want ErrPairLockedOut", err)
	}
	// Even a correct code is refused while locked.
	if _, _, err := p.Begin("phone"); err != ErrPairLockedOut {
		t.Fatalf("Begin while locked = %v, want ErrPairLockedOut", err)
	}
	// Lockout lifts with time.
	clk.advance(pairLockoutTTL + time.Second)
	p.RefreshWindow()
	if _, _, err := p.Begin("phone"); err != nil {
		t.Fatalf("Begin after lockout lifts: %v", err)
	}
}

func TestPairingAttemptExpiry(t *testing.T) {
	t.Parallel()
	p, _, clk := newPairing(t)
	p.RefreshWindow()
	id, code, _ := p.Begin("phone")

	clk.advance(pairAttemptTTL + time.Second)
	p.RefreshWindow() // keep the window open; the ATTEMPT should still lapse
	if _, err := p.Complete(code); err != ErrPairCodeMismatch {
		t.Fatalf("expired attempt Complete = %v, want mismatch", err)
	}
	if state, _ := p.PollAttempt(id); state == AttemptApproved {
		t.Fatal("expired attempt must not be approvable")
	}
}
