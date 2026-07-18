package bridge

import (
	"crypto/subtle"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/acksell/clank/pkg/auth"
)

// DeviceHeader carries the phone's self-reported model/name on bridge
// requests — cosmetic attribution ("✓ Pixel 8 connected", `clank pair
// status`), never an authorization input.
const DeviceHeader = "X-Clank-Device"

// Authenticator verifies the phone's derived bearer against the
// store's current root. Implements pkg/auth.Authenticator, so the
// bridge listener is the daemon's existing handler behind
// auth.Middleware(authenticator) — no proxy, no second API surface.
//
// It also remembers the most recent successful connection (device
// name + time), in memory only: liveness display, not audit log.
type Authenticator struct {
	store  *Store
	userID string
	log    *log.Logger

	mu         sync.Mutex
	lastDevice string
	lastSeen   time.Time
}

// NewAuthenticator wires the store to the single-user principal the
// laptop daemon runs as (same identity the old front door used).
func NewAuthenticator(store *Store, userID string, lg *log.Logger) *Authenticator {
	if lg == nil {
		lg = log.Default()
	}
	return &Authenticator{store: store, userID: userID, log: lg}
}

// Verify compares the presented bearer to the derivation of the
// current root in constant time. The first success latches
// first_connected_at — the signal that flips preview QRs from
// credential-bearing to tokenless invitations.
//
// Deriving per request (instead of caching) keeps rotation correct by
// construction; HKDF over 32 bytes is microseconds.
func (a *Authenticator) Verify(r *http.Request) (auth.Principal, error) {
	presented, err := auth.ExtractBearer(r)
	if err != nil {
		return auth.Principal{}, err
	}
	expected, err := BearerString(a.store.Root())
	if err != nil {
		return auth.Principal{}, err
	}
	if subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) != 1 {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	if err := a.store.MarkConnected(); err != nil {
		// Auth succeeded; the latch is UX state, not a gate.
		a.log.Printf("bridge: persist first-connected latch: %v", err)
	}
	a.mu.Lock()
	a.lastDevice = sanitizeDeviceName(r.Header.Get(DeviceHeader))
	a.lastSeen = time.Now()
	a.mu.Unlock()
	return auth.Principal{UserID: a.userID}, nil
}

// LastConnection reports the most recent authenticated device and
// when it was seen. Zero time = never (this daemon run).
func (a *Authenticator) LastConnection() (device string, at time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastDevice, a.lastSeen
}

// sanitizeDeviceName bounds the attacker-suppliable header: one line,
// trimmed, capped. Cosmetic only, but it lands in terminals and
// status output.
func sanitizeDeviceName(name string) string {
	name = strings.Join(strings.Fields(name), " ")
	const maxLen = 64
	if len(name) > maxLen {
		name = name[:maxLen]
	}
	if name == "" {
		return "phone"
	}
	return name
}
