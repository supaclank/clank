package bridge

import (
	"crypto/subtle"
	"log"
	"net/http"

	"github.com/acksell/clank/pkg/auth"
)

// Authenticator verifies the phone's derived bearer against the
// store's current root. Implements pkg/auth.Authenticator, so the
// bridge listener is the daemon's existing handler behind
// auth.Middleware(authenticator) — no proxy, no second API surface.
type Authenticator struct {
	store  *Store
	userID string
	log    *log.Logger
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
	return auth.Principal{UserID: a.userID}, nil
}
