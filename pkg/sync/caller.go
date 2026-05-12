package sync

import (
	"errors"
	"net/http"

	"github.com/acksell/clank/pkg/auth"
)

// CallerKind enumerates the actor types that can call the sync API.
// Mirrors OwnerKind — see its godoc on local/remote naming.
type CallerKind string

const (
	CallerKindLocal  CallerKind = "local"
	CallerKindRemote CallerKind = "remote"
)

// Caller is the authenticated identity of an inbound request to the
// /v1/worktrees + /v1/checkpoints endpoints. UserID comes from the
// auth.Principal injected by the outer auth middleware. HostID is
// set only when Kind == remote (sprite caller pushing a checkpoint,
// post-MVP).
type Caller struct {
	UserID string
	Kind   CallerKind
	HostID string // set when Kind == remote
}

// CallerVerifier extracts and validates a Caller from an HTTP request.
// Production deployments will plug in a verifier that pulls all
// values from claims; MVP uses HeaderCallerVerifier which reads
// UserID from r.Context() (via pkg/auth) and Kind/HostID from a
// request header.
type CallerVerifier interface {
	VerifyCaller(r *http.Request) (Caller, error)
}

// HeaderCallerVerifier is the default sync CallerVerifier. It reads
// UserID from the auth.Principal in r.Context() (which the outer
// auth.Middleware injected). Kind defaults to "local"; when
// X-Clank-Host-Id is present (sprite caller post-MVP), Kind flips to
// "remote" with that header's value as HostID.
//
// Stateless — no fields. Tests inject Principal via
// r.WithContext(auth.WithPrincipal(ctx, ...)).
type HeaderCallerVerifier struct{}

// HeaderHostID is the request header carrying a sprite host's ID
// (used post-MVP when sprites push their own checkpoints).
const HeaderHostID = "X-Clank-Host-Id"

// ErrNoPrincipal is returned when the request has no auth.Principal
// in context — the outer middleware didn't run. Maps to 500 not 401:
// it's a server misconfiguration, not a client problem.
var ErrNoPrincipal = errors.New("sync: no auth principal in request context")

// ErrEmptyUserID is returned when the Principal carries an empty
// UserID. Maps to 401 since the caller's identity is unverified.
var ErrEmptyUserID = errors.New("sync: empty user identity from principal")

func (v *HeaderCallerVerifier) VerifyCaller(r *http.Request) (Caller, error) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		return Caller{}, ErrNoPrincipal
	}
	if p.UserID == "" {
		return Caller{}, ErrEmptyUserID
	}
	if hostID := r.Header.Get(HeaderHostID); hostID != "" {
		return Caller{UserID: p.UserID, Kind: CallerKindRemote, HostID: hostID}, nil
	}
	return Caller{UserID: p.UserID, Kind: CallerKindLocal}, nil
}
