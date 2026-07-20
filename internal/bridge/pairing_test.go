package bridge

import (
	"crypto/ed25519"
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

// phone is a test double for the scanning device: it holds a keypair
// and drives begin→verify-reply→reveal→derive-SAS the way the app does.
type phone struct {
	priv   ed25519.PrivateKey
	pub    []byte
	nonceP []byte
}

func newPhone(t *testing.T, seed byte) *phone {
	t.Helper()
	priv := ed25519.NewKeyFromSeed(bytesRep(seed, KeyLen))
	return &phone{priv: priv, pub: priv.Public().(ed25519.PublicKey), nonceP: bytesRep(seed^0xFF, sasNonceLen)}
}

func bytesRep(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

func (ph *phone) commit() string { return SASCommit(ph.pub, ph.nonceP) }

// run drives the full handshake and returns the SAS the phone would
// display, having verified the daemon's reply against the host key.
func (ph *phone) run(t *testing.T, p *Pairing, store *Store, name string) (id, sas string) {
	t.Helper()
	id, nonceD, replySig, err := p.Begin(name, ph.commit())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if !VerifySASReply(store.HostPublicKey(), id, ph.commit(), nonceD, replySig) {
		t.Fatal("phone could not verify the daemon's reply against hk")
	}
	if err := p.Reveal(id, ph.pub, ph.nonceP); err != nil {
		t.Fatalf("Reveal: %v", err)
	}
	sas = DeriveSAS(id, ph.commit(), nonceD, ph.pub, ph.nonceP, store.HostPublicKey())
	return id, sas
}

func TestPairingHappyPath(t *testing.T) {
	t.Parallel()
	p, store, _ := newPairing(t)
	p.RefreshWindow() // a CLI is showing the QR
	ph := newPhone(t, 0x42)

	id, sas := ph.run(t, p, store, "Pixel 8")
	if len(sas) != SASDigits {
		t.Fatalf("SAS %q not %d digits", sas, SASDigits)
	}
	// Before approval: pending, and the registry doesn't know the key.
	if state := p.PollAttempt(id); state != AttemptPending {
		t.Fatalf("pre-approval poll = %s, want pending", state)
	}
	if _, ok := store.Device(ph.pub); ok {
		t.Fatal("device must not be registered before approval")
	}

	// The laptop user types the SAS — separators tolerated.
	device, err := p.Complete("  " + sas[:3] + " " + sas[3:] + " ")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if device != "Pixel 8" {
		t.Fatalf("device = %q, want Pixel 8", device)
	}
	if state := p.PollAttempt(id); state != AttemptApproved {
		t.Fatalf("post-approval poll = %s, want approved", state)
	}
	rec, ok := store.Device(ph.pub)
	if !ok || rec.Name != "Pixel 8" {
		t.Fatalf("registry after approval = %+v %v, want Pixel 8 recorded", rec, ok)
	}
}

func TestPairingBeginRejectsBadCommit(t *testing.T) {
	t.Parallel()
	p, _, _ := newPairing(t)
	p.RefreshWindow()
	nonHex := ""
	for len(nonHex) < commitHexLen {
		nonHex += "zz"
	}
	for _, bad := range []string{"", "tooshort", nonHex} {
		if _, _, _, err := p.Begin("phone", bad); err != ErrPairBadCommit {
			t.Fatalf("Begin(%q) = %v, want ErrPairBadCommit", bad, err)
		}
	}
}

func TestPairingRevealMustOpenCommit(t *testing.T) {
	t.Parallel()
	p, _, _ := newPairing(t)
	p.RefreshWindow()
	ph := newPhone(t, 0x11)

	id, _, _, err := p.Begin("phone", ph.commit())
	if err != nil {
		t.Fatal(err)
	}
	// Reveal with a different key than the commit covers — must burn it.
	other := newPhone(t, 0x22)
	if err := p.Reveal(id, other.pub, ph.nonceP); err != ErrPairCommitMismatch {
		t.Fatalf("mismatched reveal = %v, want ErrPairCommitMismatch", err)
	}
	// The attempt is gone; a correct reveal now finds nothing.
	if err := p.Reveal(id, ph.pub, ph.nonceP); err != ErrPairNoAttempt {
		t.Fatalf("reveal after burn = %v, want ErrPairNoAttempt", err)
	}
}

func TestPairingRevealValidatesInputs(t *testing.T) {
	t.Parallel()
	p, _, _ := newPairing(t)
	p.RefreshWindow()
	ph := newPhone(t, 0x33)
	id, _, _, _ := p.Begin("phone", ph.commit())
	if err := p.Reveal(id, ph.pub[:16], ph.nonceP); err != ErrPairBadKey {
		t.Fatalf("short pubkey = %v, want ErrPairBadKey", err)
	}
	if err := p.Reveal(id, ph.pub, ph.nonceP[:8]); err != ErrPairBadKey {
		t.Fatalf("short nonce = %v, want ErrPairBadKey", err)
	}
	if err := p.Reveal("nope", ph.pub, ph.nonceP); err != ErrPairNoAttempt {
		t.Fatalf("unknown id = %v, want ErrPairNoAttempt", err)
	}
}

func TestPairingWindowGating(t *testing.T) {
	t.Parallel()
	p, _, _ := newPairing(t)
	ph := newPhone(t, 0x01)

	// No window yet → Begin refused.
	if _, _, _, err := p.Begin("phone", ph.commit()); err != ErrPairWindowClosed {
		t.Fatalf("Begin with no window = %v, want ErrPairWindowClosed", err)
	}
	p.RefreshWindow()
	if _, _, _, err := p.Begin("phone", ph.commit()); err != nil {
		t.Fatalf("Begin in window: %v", err)
	}
}

func TestPairingWindowLeaseExpires(t *testing.T) {
	t.Parallel()
	p, _, clk := newPairing(t)
	ph := newPhone(t, 0x02)
	p.RefreshWindow()
	clk.advance(pairWindowLease + time.Second)
	if _, _, _, err := p.Begin("phone", ph.commit()); err != ErrPairWindowClosed {
		t.Fatalf("Begin after lease = %v, want ErrPairWindowClosed", err)
	}
}

// TestPairingSASSelectsAttempt pins the core rule: with two phones
// waiting, the typed SAS approves the one whose transcript derives it —
// device names never gate approval, and only that phone's key lands in
// the registry.
func TestPairingSASSelectsAttempt(t *testing.T) {
	t.Parallel()
	p, store, _ := newPairing(t)
	p.RefreshWindow()
	attacker := newPhone(t, 0xEE)
	mine := newPhone(t, 0x1A)

	_, sasAtt := attacker.run(t, p, store, "Attacker Device")
	idMine, sasMine := mine.run(t, p, store, "My Pixel")
	if sasAtt == sasMine {
		t.Skip("astronomically unlikely SAS collision in vectors; rerun")
	}

	device, err := p.Complete(sasMine)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if device != "My Pixel" {
		t.Fatalf("approved %q, want My Pixel", device)
	}
	if state := p.PollAttempt(idMine); state != AttemptApproved {
		t.Fatal("my attempt should be approved")
	}
	if _, ok := store.Device(attacker.pub); ok {
		t.Fatal("attacker key must not land in the registry")
	}
	if _, ok := store.Device(mine.pub); !ok {
		t.Fatal("my key must be registered")
	}
}

// TestPairingAmbiguousSASExpiresBoth pins the collision rule: a SAS
// matching two revealed attempts cancels both (a derived code can't be
// redrawn to disambiguate). White-box: force the collision.
func TestPairingAmbiguousSASExpiresBoth(t *testing.T) {
	t.Parallel()
	p, store, _ := newPairing(t)
	p.RefreshWindow()
	a := newPhone(t, 0x51)
	b := newPhone(t, 0x52)
	a.run(t, p, store, "Phone A")
	b.run(t, p, store, "Phone B")

	p.mu.Lock()
	for _, at := range p.attempts {
		at.sas = "424242"
	}
	p.mu.Unlock()

	if _, err := p.Complete("424242"); err != ErrPairAmbiguous {
		t.Fatalf("ambiguous Complete = %v, want ErrPairAmbiguous", err)
	}
	// Both gone; neither key enrolled.
	if _, ok := store.Device(a.pub); ok {
		t.Error("Phone A must not be enrolled after an ambiguous match")
	}
	if _, ok := store.Device(b.pub); ok {
		t.Error("Phone B must not be enrolled after an ambiguous match")
	}
	if _, err := p.Complete("424242"); err != ErrPairCodeMismatch {
		t.Fatalf("after both expired = %v, want ErrPairCodeMismatch", err)
	}
}

func TestPairingPendingCap(t *testing.T) {
	t.Parallel()
	p, _, _ := newPairing(t)
	p.RefreshWindow()
	for i := 0; i < pairMaxPending; i++ {
		if _, _, _, err := p.Begin("phone", newPhone(t, byte(i+1)).commit()); err != nil {
			t.Fatalf("Begin %d: %v", i, err)
		}
	}
	if _, _, _, err := p.Begin("one too many", newPhone(t, 0x99).commit()); err != ErrPairTooManyPending {
		t.Fatalf("over-cap Begin = %v, want ErrPairTooManyPending", err)
	}
}

func TestPairingLockout(t *testing.T) {
	t.Parallel()
	p, store, clk := newPairing(t)
	p.RefreshWindow()
	newPhone(t, 0x01).run(t, p, store, "phone")

	for i := 0; i < pairMaxWrong; i++ {
		if _, err := p.Complete("000000"); err != ErrPairCodeMismatch {
			t.Fatalf("wrong %d = %v, want mismatch", i, err)
		}
		p.RefreshWindow() // per-second poll must NOT reset the counter
	}
	if _, err := p.Complete("000000"); err != ErrPairLockedOut {
		t.Fatalf("post-lockout = %v, want ErrPairLockedOut", err)
	}
	// Even a fresh Begin is refused while locked.
	if _, _, _, err := p.Begin("phone", newPhone(t, 0x02).commit()); err != ErrPairLockedOut {
		t.Fatalf("Begin while locked = %v, want ErrPairLockedOut", err)
	}
	// Lockout lifts with time.
	clk.advance(pairLockoutTTL + time.Second)
	p.RefreshWindow()
	if _, _, _, err := p.Begin("phone", newPhone(t, 0x03).commit()); err != nil {
		t.Fatalf("Begin after lockout lifts: %v", err)
	}
}

func TestPairingSuccessResetsWrongCounter(t *testing.T) {
	t.Parallel()
	p, store, _ := newPairing(t)
	p.RefreshWindow()
	ph := newPhone(t, 0x01)
	_, sas := ph.run(t, p, store, "phone")

	for i := 0; i < pairMaxWrong-1; i++ {
		if _, err := p.Complete("000000"); err != ErrPairCodeMismatch {
			t.Fatalf("wrong %d = %v, want mismatch", i, err)
		}
	}
	if _, err := p.Complete(sas); err != nil {
		t.Fatalf("Complete(sas): %v", err)
	}
	if p.wrong != 0 {
		t.Fatalf("wrong = %d after a successful pairing, want 0 — a stale count must not carry into the next one", p.wrong)
	}
}

func TestPairingAttemptExpiry(t *testing.T) {
	t.Parallel()
	p, store, clk := newPairing(t)
	p.RefreshWindow()
	ph := newPhone(t, 0x77)
	id, sas := ph.run(t, p, store, "phone")

	clk.advance(pairAttemptTTL + time.Second)
	p.RefreshWindow() // keep the window open; the ATTEMPT should still lapse
	if _, err := p.Complete(sas); err != ErrPairCodeMismatch {
		t.Fatalf("expired attempt Complete = %v, want mismatch", err)
	}
	if state := p.PollAttempt(id); state == AttemptApproved {
		t.Fatal("expired attempt must not be approvable")
	}
	if _, ok := store.Device(ph.pub); ok {
		t.Fatal("expired attempt must not register a key")
	}
}

// TestPairingOnlyPromptableAfterReveal pins that a begun-but-unrevealed
// attempt is invisible to the laptop (no SAS to type yet).
func TestPairingOnlyPromptableAfterReveal(t *testing.T) {
	t.Parallel()
	p, _, _ := newPairing(t)
	p.RefreshWindow()
	ph := newPhone(t, 0x61)
	if _, _, _, err := p.Begin("Lonely Phone", ph.commit()); err != nil {
		t.Fatal(err)
	}
	if names := p.RefreshWindow(); len(names) != 0 {
		t.Fatalf("pre-reveal RefreshWindow = %v, want empty", names)
	}
}
