package bridge

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"
)

// Pairing is the typed-code approval ceremony: a new phone scans the
// (tokenless) QR, POSTs its device name, and gets a 6-digit code it
// displays; the laptop user types that code to release the root secret
// to that phone. The typed code SELECTS the attempt — device names are
// cosmetic and never gate approval, so a stranger's concurrent attempt
// can't harvest your keystroke.
//
// The window is open only while a CLI is showing the QR (it leases the
// window by polling); outside that, Begin is refused. Delivery hands
// the root secret over the wire exactly once.
type Pairing struct {
	store *Store
	now   func() time.Time

	mu          sync.Mutex
	windowUntil time.Time
	lockedUntil time.Time
	wrong       int
	attempts    []*pairAttempt
}

// Ceremony guardrails.
const (
	pairCodeDigits = 6
	// pairAttemptTTL is how long a displayed code stays claimable.
	pairAttemptTTL = 2 * time.Minute
	// pairWindowLease is how long Begin keeps accepting after the last
	// CLI poll — the window closes shortly after the QR stops showing.
	pairWindowLease = 30 * time.Second
	// pairMaxPending bounds concurrent unapproved attempts so a hostile
	// LAN can't flood the code space hoping a typo lands.
	pairMaxPending = 3
	// pairMaxWrong wrong codes trips a time-based lockout — time-based
	// (not reset-on-poll) so the CLI's per-second window refresh can't
	// wipe the counter.
	pairMaxWrong   = 5
	pairLockoutTTL = 30 * time.Second
)

var (
	// ErrPairWindowClosed: Begin with no CLI showing the QR.
	ErrPairWindowClosed = errors.New("bridge: no pairing window open — run `clank pair` or `clank preview` on the laptop")
	// ErrPairTooManyPending: pending-attempt cap reached.
	ErrPairTooManyPending = errors.New("bridge: too many pending pairing attempts — wait a moment and rescan")
	// ErrPairLockedOut: too many wrong codes; temporarily locked.
	ErrPairLockedOut = errors.New("bridge: pairing locked after repeated wrong codes — wait a moment")
	// ErrPairCodeMismatch: typed code matched no pending attempt.
	ErrPairCodeMismatch = errors.New("bridge: that code matches no waiting phone")
)

// AttemptState is the phone-visible lifecycle of one attempt.
type AttemptState string

const (
	AttemptPending  AttemptState = "pending"
	AttemptApproved AttemptState = "approved"
	AttemptExpired  AttemptState = "expired"
)

type pairAttempt struct {
	id        string
	code      string
	device    string
	expiresAt time.Time
	approved  bool
	delivered bool
}

func (a *pairAttempt) pending(now time.Time) bool {
	return !a.approved && now.Before(a.expiresAt)
}

// NewPairing wires the ceremony to the secret store. now==nil uses the
// wall clock.
func NewPairing(store *Store, now func() time.Time) *Pairing {
	if now == nil {
		now = time.Now
	}
	return &Pairing{store: store, now: now}
}

// RefreshWindow keeps the pairing window open — the CLI calls it each
// tick while the QR is up — and returns the device names of phones
// currently waiting for approval (so the prompt can name them).
func (p *Pairing) RefreshWindow() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.windowUntil = p.now().Add(pairWindowLease)
	p.gcLocked()
	var devices []string
	now := p.now()
	for _, a := range p.attempts {
		if a.pending(now) {
			devices = append(devices, a.device)
		}
	}
	return devices
}

// Begin registers a scanning phone and returns its attempt id + the
// code it must display. Pre-auth by nature — window gating, the
// pending cap, and the lockout are the whole defense.
func (p *Pairing) Begin(device string) (id, code string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gcLocked()
	now := p.now()
	if !now.Before(p.windowUntil) {
		return "", "", ErrPairWindowClosed
	}
	if now.Before(p.lockedUntil) {
		return "", "", ErrPairLockedOut
	}
	pending := 0
	for _, a := range p.attempts {
		if a.pending(now) {
			pending++
		}
	}
	if pending >= pairMaxPending {
		return "", "", ErrPairTooManyPending
	}
	code, err = p.uniqueCodeLocked()
	if err != nil {
		return "", "", err
	}
	id, err = randomHex(16)
	if err != nil {
		return "", "", err
	}
	p.attempts = append(p.attempts, &pairAttempt{
		id:        id,
		code:      code,
		device:    sanitizeDeviceName(device),
		expiresAt: now.Add(pairAttemptTTL),
	})
	return id, code, nil
}

// Complete consumes a code typed at the laptop, approving the matching
// pending attempt. A miss burns one wrong-entry; enough of them trip a
// time-based lockout.
func (p *Pairing) Complete(typed string) (device string, err error) {
	code := normalizeCode(typed)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gcLocked()
	now := p.now()
	if now.Before(p.lockedUntil) {
		return "", ErrPairLockedOut
	}
	for _, a := range p.attempts {
		if a.pending(now) && a.code == code {
			a.approved = true
			return a.device, nil
		}
	}
	p.wrong++
	if p.wrong >= pairMaxWrong {
		p.lockedUntil = now.Add(pairLockoutTTL)
		p.wrong = 0
	}
	return "", ErrPairCodeMismatch
}

// PollAttempt reports an attempt's state to the polling phone. The
// first poll after approval delivers the root secret and marks it
// delivered — the daemon never hands the secret out twice, and holds
// it in the clear no longer than the delivery.
func (p *Pairing) PollAttempt(id string) (state AttemptState, secret []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gcLocked()
	now := p.now()
	for _, a := range p.attempts {
		if a.id != id {
			continue
		}
		switch {
		case a.approved && !a.delivered:
			a.delivered = true
			return AttemptApproved, p.store.Root()
		case a.approved:
			return AttemptApproved, nil
		case a.pending(now):
			return AttemptPending, nil
		}
	}
	return AttemptExpired, nil
}

// gcLocked drops attempts past a hard lifetime cap (2×TTL from
// creation: pending expiry plus a grace for the phone to observe
// approval + fetch the secret). Caller holds p.mu.
func (p *Pairing) gcLocked() {
	now := p.now()
	kept := p.attempts[:0]
	for _, a := range p.attempts {
		if now.After(a.expiresAt.Add(pairAttemptTTL)) {
			continue
		}
		kept = append(kept, a)
	}
	p.attempts = kept
}

// uniqueCodeLocked draws a crypto-random code distinct from every
// pending one, so a typed code can never select two attempts.
func (p *Pairing) uniqueCodeLocked() (string, error) {
	now := p.now()
	for range 10 {
		code, err := randomCode()
		if err != nil {
			return "", err
		}
		collides := false
		for _, a := range p.attempts {
			if a.pending(now) && a.code == code {
				collides = true
				break
			}
		}
		if !collides {
			return code, nil
		}
	}
	return "", fmt.Errorf("bridge: could not draw a unique pairing code")
}

func randomCode() (string, error) {
	max := big.NewInt(1)
	for range pairCodeDigits {
		max.Mul(max, big.NewInt(10))
	}
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", fmt.Errorf("bridge: generate code: %w", err)
	}
	return fmt.Sprintf("%0*d", pairCodeDigits, n), nil
}

func randomHex(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("bridge: generate id: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// normalizeCode strips the separators humans type ("731 442",
// "731-442") down to the digits that identify the attempt.
func normalizeCode(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r >= '0' && r <= '9' {
			out = append(out, r)
		}
	}
	return string(out)
}
