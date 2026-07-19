package bridge

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/acksell/clank/pkg/auth"
)

// sigSkewWindow bounds how far a request timestamp may drift from the
// daemon clock in either direction. Phones are NTP-synced; a client
// that drifts further can resync from the response's standard Date
// header and retry.
const sigSkewWindow = 2 * time.Minute

// nonceSweepThreshold caps the replay cache before expired entries are
// swept — housekeeping, not a security bound.
const nonceSweepThreshold = 4096

// Authenticator verifies per-request Ed25519 signatures against the
// approved-device registry. Implements pkg/auth.Authenticator, so the
// bridge listener is the daemon's existing handler behind
// auth.Middleware(authenticator) — no proxy, no second API surface.
//
// It also remembers the most recent successful connection (device
// name + time), in memory only: liveness display, not audit log.
type Authenticator struct {
	store  *Store
	userID string
	log    *log.Logger
	now    func() time.Time

	mu         sync.Mutex
	nonces     map[string]time.Time
	lastDevice string
	lastSeen   time.Time
}

// NewAuthenticator wires the registry to the single-user principal the
// laptop daemon runs as (same identity the old front door used).
// now==nil uses the wall clock.
func NewAuthenticator(store *Store, userID string, lg *log.Logger, now func() time.Time) *Authenticator {
	if lg == nil {
		lg = log.Default()
	}
	if now == nil {
		now = time.Now
	}
	return &Authenticator{store: store, userID: userID, log: lg, now: now, nonces: make(map[string]time.Time)}
}

// Verify checks the four signature headers: the key must be an
// approved device, the timestamp fresh, the nonce unseen, and the
// signature valid over the canonical request (which covers the body —
// read here and restored for the downstream handler). Every failure is
// the same ErrUnauthenticated: an unpaired probe learns nothing about
// which check tripped.
func (a *Authenticator) Verify(r *http.Request) (auth.Principal, error) {
	pub, err := DecodeKey(r.Header.Get(HeaderKey))
	if err != nil {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	rec, ok := a.store.Device(pub)
	if !ok {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	ts, err := strconv.ParseInt(r.Header.Get(HeaderTimestamp), 10, 64)
	if err != nil {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	now := a.now()
	if drift := now.Sub(time.Unix(ts, 0)); drift > sigSkewWindow || drift < -sigSkewWindow {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	nonce := r.Header.Get(HeaderNonce)
	if raw, err := hex.DecodeString(nonce); err != nil || len(raw) != sigNonceLen {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	sig, err := DecodeSig(r.Header.Get(HeaderSignature))
	if err != nil {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	// Reserve the nonce before the signature check so two concurrent
	// replays can't both pass; a reservation burned by a bad signature
	// only blocks whoever chose that nonce.
	if !a.reserveNonce(nonce, now) {
		return auth.Principal{}, auth.ErrUnauthenticated
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	canonical := CanonicalRequest(ts, nonce, r.Method, requestURI(r), body)
	if !ed25519.Verify(ed25519.PublicKey(pub), canonical, sig) {
		return auth.Principal{}, auth.ErrUnauthenticated
	}

	if err := a.store.TouchDevice(pub); err != nil {
		// Auth succeeded; last_seen is display state, not a gate.
		a.log.Printf("bridge: touch device: %v", err)
	}
	a.mu.Lock()
	a.lastDevice = rec.Name
	a.lastSeen = now
	a.mu.Unlock()
	return auth.Principal{UserID: a.userID}, nil
}

// reserveNonce records a nonce as seen, reporting false on replay.
// Entries expire after the skew window closes on both sides.
func (a *Authenticator) reserveNonce(nonce string, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if expiry, seen := a.nonces[nonce]; seen && now.Before(expiry) {
		return false
	}
	if len(a.nonces) >= nonceSweepThreshold {
		for n, expiry := range a.nonces {
			if !now.Before(expiry) {
				delete(a.nonces, n)
			}
		}
	}
	a.nonces[nonce] = now.Add(2 * sigSkewWindow)
	return true
}

// LastConnection reports the most recent authenticated device and
// when it was seen. Zero time = never (this daemon run).
func (a *Authenticator) LastConnection() (device string, at time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastDevice, a.lastSeen
}

// requestURI is the exact request-target the client signed: the raw
// bytes from the request line when serving, the reconstructed form for
// in-process tests.
func requestURI(r *http.Request) string {
	if r.RequestURI != "" {
		return r.RequestURI
	}
	return r.URL.RequestURI()
}
