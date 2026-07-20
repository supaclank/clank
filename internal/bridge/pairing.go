package bridge

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Pairing is the SAS approval ceremony. A new phone scans the QR
// (addresses + the laptop's host public key, hk), then runs a
// commit-then-reveal handshake so the 6-digit code the user types
// authenticates WHICH device key gets enrolled — closing the
// active-MITM hole at pairing without a TLS pin (see sas.go):
//
//  1. Begin: phone sends its name + a commit = H(device_pub ‖ nonce_P).
//     The daemon picks nonce_D, opens an attempt, and returns nonce_D
//     plus a host-key signature over (attempt ‖ commit ‖ nonce_D). The
//     phone verifies that against hk before trusting anything.
//  2. Reveal: phone sends device_pub + nonce_P; the daemon checks they
//     open the commit and derives the SAS. Both sides now hold the same
//     6 digits — the phone displays them, never sending them.
//  3. Complete: the laptop user types the SAS; the daemon approves the
//     revealed attempt whose derived SAS matches and records its key.
//
// Nothing secret ever crosses the wire. The window is open only while a
// CLI is showing the QR (it leases the window by polling).
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
	// commitHexLen is the wire length of a SHA-256 commit (32 bytes).
	commitHexLen = 64
	// pairAttemptTTL is how long an attempt stays claimable.
	pairAttemptTTL = 2 * time.Minute
	// pairWindowLease is how long Begin keeps accepting after the last
	// CLI poll — the window closes shortly after the QR stops showing.
	pairWindowLease = 30 * time.Second
	// pairMaxPending bounds concurrent unapproved attempts so a hostile
	// LAN can't flood the handshake hoping a SAS collision lands.
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
	// ErrPairCodeMismatch: typed code matched no waiting phone.
	ErrPairCodeMismatch = errors.New("bridge: that code matches no waiting phone")
	// ErrPairAmbiguous: typed code matched more than one attempt; both
	// are expired and the phones must rescan (a derived SAS can't be
	// redrawn to break the tie).
	ErrPairAmbiguous = errors.New("bridge: that code matched two phones — both were cancelled, please rescan")
	// ErrPairBadCommit: Begin without a valid commitment.
	ErrPairBadCommit = errors.New("bridge: pairing requires a valid commitment")
	// ErrPairBadKey: Reveal without a valid device public key/nonce.
	ErrPairBadKey = errors.New("bridge: pairing reveal requires the phone's public key")
	// ErrPairNoAttempt: Reveal for an unknown or expired attempt.
	ErrPairNoAttempt = errors.New("bridge: pairing attempt not found or expired — rescan")
	// ErrPairCommitMismatch: revealed values don't open the commitment.
	ErrPairCommitMismatch = errors.New("bridge: pairing reveal did not match the commitment — rescan")
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
	device    string
	commit    string
	nonceD    []byte
	expiresAt time.Time

	// Set at Reveal, once the phone opens its commit.
	revealed bool
	pubkey   []byte
	sas      string

	approved bool
}

// live reports whether an attempt still counts (not approved, not
// expired) — for the pending cap and gc grace.
func (a *pairAttempt) live(now time.Time) bool {
	return !a.approved && now.Before(a.expiresAt)
}

// promptable reports whether the laptop user can act on this attempt:
// live and past reveal, so a SAS exists to type.
func (a *pairAttempt) promptable(now time.Time) bool {
	return a.revealed && a.live(now)
}

// NewPairing wires the ceremony to the device registry. now==nil uses
// the wall clock.
func NewPairing(store *Store, now func() time.Time) *Pairing {
	if now == nil {
		now = time.Now
	}
	return &Pairing{store: store, now: now}
}

// RefreshWindow keeps the pairing window open — the CLI calls it each
// tick while the QR is up — and returns the device names of phones that
// have revealed and are waiting for the user to type their code.
func (p *Pairing) RefreshWindow() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.windowUntil = p.now().Add(pairWindowLease)
	p.gcLocked()
	var devices []string
	now := p.now()
	for _, a := range p.attempts {
		if a.promptable(now) {
			devices = append(devices, a.device)
		}
	}
	return devices
}

// Begin opens an attempt for a scanning phone: it records the phone's
// name + commitment, picks the daemon nonce, and returns that nonce
// with a host-key signature the phone verifies against hk. Pre-auth by
// nature — window gating, the pending cap, and the lockout are the
// whole defense until the SAS is typed.
func (p *Pairing) Begin(device, commitHex string) (id string, nonceD []byte, replySig string, err error) {
	if len(commitHex) != commitHexLen {
		return "", nil, "", ErrPairBadCommit
	}
	if _, decErr := hex.DecodeString(commitHex); decErr != nil {
		return "", nil, "", ErrPairBadCommit
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gcLocked()
	now := p.now()
	if !now.Before(p.windowUntil) {
		return "", nil, "", ErrPairWindowClosed
	}
	if now.Before(p.lockedUntil) {
		return "", nil, "", ErrPairLockedOut
	}
	live := 0
	for _, a := range p.attempts {
		if a.live(now) {
			live++
		}
	}
	if live >= pairMaxPending {
		return "", nil, "", ErrPairTooManyPending
	}
	id, err = randomHex(16)
	if err != nil {
		return "", nil, "", err
	}
	nonceD, err = randomBytes(sasNonceLen)
	if err != nil {
		return "", nil, "", err
	}
	p.attempts = append(p.attempts, &pairAttempt{
		id:        id,
		device:    sanitizeDeviceName(device),
		commit:    commitHex,
		nonceD:    nonceD,
		expiresAt: now.Add(pairAttemptTTL),
	})
	return id, nonceD, EncodeSig(p.store.SignSASReply(id, commitHex, nonceD)), nil
}

// Reveal opens the phone's commit: it verifies device_pub + nonce_P
// hash to the stored commit, then derives and stores the SAS. A reveal
// that doesn't open the commit burns the attempt.
func (p *Pairing) Reveal(id string, devicePub, nonceP []byte) error {
	if len(devicePub) != KeyLen || len(nonceP) != sasNonceLen {
		return ErrPairBadKey
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gcLocked()
	now := p.now()
	for _, a := range p.attempts {
		if a.id != id || !a.live(now) {
			continue
		}
		if a.revealed {
			return nil // idempotent — a retried reveal is fine
		}
		if SASCommit(devicePub, nonceP) != a.commit {
			p.removeLocked(id)
			return ErrPairCommitMismatch
		}
		a.pubkey = devicePub
		a.revealed = true
		a.sas = DeriveSAS(a.id, a.commit, a.nonceD, devicePub, nonceP, p.store.HostPublicKey())
		return nil
	}
	return ErrPairNoAttempt
}

// Complete consumes the SAS typed at the laptop, approving the revealed
// attempt whose derived SAS matches by recording its public key. A miss
// burns one wrong-entry; a code matching two attempts expires both (a
// derived SAS can't be redrawn to break the tie).
func (p *Pairing) Complete(typed string) (device string, err error) {
	code := normalizeCode(typed)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gcLocked()
	now := p.now()
	if now.Before(p.lockedUntil) {
		return "", ErrPairLockedOut
	}
	var matches []*pairAttempt
	for _, a := range p.attempts {
		if a.promptable(now) && a.sas == code {
			matches = append(matches, a)
		}
	}
	switch len(matches) {
	case 1:
		a := matches[0]
		// Record before approving: an approval the registry never saw
		// would leave the phone signing into 401s.
		if err := p.store.AddDevice(a.pubkey, a.device); err != nil {
			return "", err
		}
		a.approved = true
		p.wrong = 0 // a clean pairing must not carry stale mismatches into the next one
		return a.device, nil
	case 0:
		p.wrong++
		if p.wrong >= pairMaxWrong {
			p.lockedUntil = now.Add(pairLockoutTTL)
			p.wrong = 0
		}
		return "", ErrPairCodeMismatch
	default:
		for _, a := range matches {
			p.removeLocked(a.id)
		}
		return "", ErrPairAmbiguous
	}
}

// PollAttempt reports an attempt's state to the polling phone.
// Approval carries no payload — the phone's own key is now trusted,
// and its next signed request just works.
func (p *Pairing) PollAttempt(id string) AttemptState {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gcLocked()
	now := p.now()
	for _, a := range p.attempts {
		if a.id != id {
			continue
		}
		switch {
		case a.approved:
			return AttemptApproved
		case a.live(now):
			return AttemptPending
		}
	}
	return AttemptExpired
}

// gcLocked drops attempts past a hard lifetime cap (2×TTL from
// creation: expiry plus a grace for the phone to observe approval).
// Caller holds p.mu.
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

// removeLocked drops one attempt by id. Caller holds p.mu.
func (p *Pairing) removeLocked(id string) {
	kept := p.attempts[:0]
	for _, a := range p.attempts {
		if a.id != id {
			kept = append(kept, a)
		}
	}
	p.attempts = kept
}

func randomHex(n int) (string, error) {
	raw, err := randomBytes(n)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func randomBytes(n int) ([]byte, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("bridge: generate random: %w", err)
	}
	return raw, nil
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

// sanitizeDeviceName bounds the phone-suppliable name: one line,
// trimmed, capped. Cosmetic only, but it lands in terminals and
// status output.
func sanitizeDeviceName(name string) string {
	name = strings.Join(strings.Fields(name), " ")
	const maxLen = 64
	if runes := []rune(name); len(runes) > maxLen {
		name = string(runes[:maxLen])
	}
	if name == "" {
		return "phone"
	}
	return name
}
