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

// testPub returns a distinct valid device public key per fill byte.
func testPub(fill byte) []byte {
	return bytes.Repeat([]byte{fill}, KeyLen)
}

func TestPairingHappyPath(t *testing.T) {
	t.Parallel()
	p, store, _ := newPairing(t)
	p.RefreshWindow() // a CLI is showing the QR

	pub := testPub(0x42)
	id, code, err := p.Begin("Pixel 8", pub)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if len(code) != pairCodeDigits {
		t.Fatalf("code %q not %d digits", code, pairCodeDigits)
	}

	// Before approval: pending, and the registry doesn't know the key.
	if state := p.PollAttempt(id); state != AttemptPending {
		t.Fatalf("pre-approval poll = %s, want pending", state)
	}
	if _, ok := store.Device(pub); ok {
		t.Fatal("device must not be registered before approval")
	}

	// The laptop user types the code — separators tolerated.
	device, err := p.Complete("  " + code[:3] + " " + code[3:] + " ")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if device != "Pixel 8" {
		t.Fatalf("device = %q, want Pixel 8", device)
	}

	// Approval = the key is in the registry; the poll carries no payload.
	if state := p.PollAttempt(id); state != AttemptApproved {
		t.Fatalf("post-approval poll = %s, want approved", state)
	}
	rec, ok := store.Device(pub)
	if !ok || rec.Name != "Pixel 8" {
		t.Fatalf("registry after approval = %+v %v, want Pixel 8 recorded", rec, ok)
	}
}

func TestPairingBeginRequiresKey(t *testing.T) {
	t.Parallel()
	p, _, _ := newPairing(t)
	p.RefreshWindow()
	if _, _, err := p.Begin("phone", nil); err != ErrPairBadKey {
		t.Fatalf("Begin(nil key) = %v, want ErrPairBadKey", err)
	}
	if _, _, err := p.Begin("phone", []byte("short")); err != ErrPairBadKey {
		t.Fatalf("Begin(short key) = %v, want ErrPairBadKey", err)
	}
}

func TestPairingWindowGating(t *testing.T) {
	t.Parallel()
	p, _, clk := newPairing(t)

	// No window yet → Begin refused.
	if _, _, err := p.Begin("phone", testPub(1)); err != ErrPairWindowClosed {
		t.Fatalf("Begin with no window = %v, want ErrPairWindowClosed", err)
	}

	p.RefreshWindow()
	if _, _, err := p.Begin("phone", testPub(1)); err != nil {
		t.Fatalf("Begin in window: %v", err)
	}

	// Lease expires without a refresh → window closes.
	clk.advance(pairWindowLease + time.Second)
	if _, _, err := p.Begin("phone2", testPub(2)); err != ErrPairWindowClosed {
		t.Fatalf("Begin after lease = %v, want ErrPairWindowClosed", err)
	}
}

// TestPairingTypedCodeSelectsAttempt pins the core rule: with two
// phones waiting, the typed code approves the one that holds it —
// device names never gate approval, and only the approved phone's key
// lands in the registry.
func TestPairingTypedCodeSelectsAttempt(t *testing.T) {
	t.Parallel()
	p, store, _ := newPairing(t)
	p.RefreshWindow()

	attackerPub, minePub := testPub(0xEE), testPub(0x11)
	idA, codeA, _ := p.Begin("Attacker Device", attackerPub)
	idB, codeB, _ := p.Begin("My Pixel", minePub)
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
	if state := p.PollAttempt(idB); state != AttemptApproved {
		t.Fatal("my attempt should be approved")
	}
	if state := p.PollAttempt(idA); state != AttemptPending {
		t.Fatalf("attacker attempt = %s, want still pending", state)
	}
	if _, ok := store.Device(attackerPub); ok {
		t.Fatal("attacker key must not land in the registry")
	}
	if _, ok := store.Device(minePub); !ok {
		t.Fatal("my key must be registered")
	}
}

func TestPairingPendingCap(t *testing.T) {
	t.Parallel()
	p, _, _ := newPairing(t)
	p.RefreshWindow()
	for i := 0; i < pairMaxPending; i++ {
		if _, _, err := p.Begin("phone", testPub(byte(i+1))); err != nil {
			t.Fatalf("Begin %d: %v", i, err)
		}
	}
	if _, _, err := p.Begin("one too many", testPub(0x99)); err != ErrPairTooManyPending {
		t.Fatalf("over-cap Begin = %v, want ErrPairTooManyPending", err)
	}
}

func TestPairingLockout(t *testing.T) {
	t.Parallel()
	p, _, clk := newPairing(t)
	p.RefreshWindow()
	p.Begin("phone", testPub(1))

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
	if _, _, err := p.Begin("phone", testPub(2)); err != ErrPairLockedOut {
		t.Fatalf("Begin while locked = %v, want ErrPairLockedOut", err)
	}
	// Lockout lifts with time.
	clk.advance(pairLockoutTTL + time.Second)
	p.RefreshWindow()
	if _, _, err := p.Begin("phone", testPub(3)); err != nil {
		t.Fatalf("Begin after lockout lifts: %v", err)
	}
}

func TestPairingAttemptExpiry(t *testing.T) {
	t.Parallel()
	p, store, clk := newPairing(t)
	p.RefreshWindow()
	pub := testPub(0x77)
	id, code, _ := p.Begin("phone", pub)

	clk.advance(pairAttemptTTL + time.Second)
	p.RefreshWindow() // keep the window open; the ATTEMPT should still lapse
	if _, err := p.Complete(code); err != ErrPairCodeMismatch {
		t.Fatalf("expired attempt Complete = %v, want mismatch", err)
	}
	if state := p.PollAttempt(id); state == AttemptApproved {
		t.Fatal("expired attempt must not be approvable")
	}
	if _, ok := store.Device(pub); ok {
		t.Fatal("expired attempt must not register a key")
	}
}
