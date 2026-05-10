package sync

import (
	"errors"
	"fmt"
	"net/http"
)

// CallerKind enumerates the actor types that can call the sync API.
// Mirrors OwnerKind — see its godoc on local/remote naming.
type CallerKind string

const (
	CallerKindLocal  CallerKind = "local"
	CallerKindRemote CallerKind = "remote"
)

// Caller is the authenticated identity of an inbound request to the
// /v1/worktrees + /v1/checkpoints endpoints. UserID always comes from
// verified claims; DeviceID or HostID identifies the specific device
// or remote host (exactly one is non-empty per Kind).
type Caller struct {
	UserID   string
	Kind     CallerKind
	DeviceID string // set when Kind == local
	HostID   string // set when Kind == remote
}

// CallerVerifier extracts and validates a Caller from an HTTP request.
// Production deployments will plug in a JWT verifier that pulls
// device_id/host_id from claims; MVP uses HeaderCallerVerifier which
// trusts request headers (combined with the existing Authenticator
// for userID).
type CallerVerifier interface {
	VerifyCaller(r *http.Request) (Caller, error)
}

// HeaderCallerVerifier wraps an existing Authenticator + UserIDFromClaims
// and reads device_id / host_id from request headers. Used for MVP
// pre-JWT deployments and tests. P2 follow-up: a JWT verifier that
// puts device_id/host_id in claims.
type HeaderCallerVerifier struct {
	Auth             Authenticator
	UserIDFromClaims func(claims map[string]any) (string, error)
}

// HeaderDeviceID is the request header carrying the laptop's device
// identifier. Pinned as a constant so client and server agree on the
// exact spelling.
const HeaderDeviceID = "X-Clank-Device-Id"

// HeaderHostID is the request header carrying a sprite host's ID
// (used post-MVP when sprites push their own checkpoints).
const HeaderHostID = "X-Clank-Host-Id"

// ErrNoCallerIdentity is returned by VerifyCaller when neither
// X-Clank-Device-Id nor X-Clank-Host-Id is present.
var ErrNoCallerIdentity = errors.New("sync: missing X-Clank-Device-Id or X-Clank-Host-Id header")

// ErrAmbiguousCaller is returned when both headers are present —
// callers must declare exactly one identity per request.
var ErrAmbiguousCaller = errors.New("sync: cannot specify both X-Clank-Device-Id and X-Clank-Host-Id")

func (v *HeaderCallerVerifier) VerifyCaller(r *http.Request) (Caller, error) {
	if v.Auth == nil || v.UserIDFromClaims == nil {
		return Caller{}, errors.New("sync: HeaderCallerVerifier needs Auth and UserIDFromClaims")
	}
	claims, err := v.Auth.Verify(r)
	if err != nil {
		return Caller{}, err
	}
	userID, err := v.UserIDFromClaims(claims)
	if err != nil {
		return Caller{}, fmt.Errorf("user identity: %w", err)
	}
	if userID == "" {
		return Caller{}, errors.New("sync: empty user identity from claims")
	}

	deviceID := r.Header.Get(HeaderDeviceID)
	hostID := r.Header.Get(HeaderHostID)
	switch {
	case deviceID != "" && hostID != "":
		return Caller{}, ErrAmbiguousCaller
	case deviceID != "":
		return Caller{UserID: userID, Kind: CallerKindLocal, DeviceID: deviceID}, nil
	case hostID != "":
		return Caller{UserID: userID, Kind: CallerKindRemote, HostID: hostID}, nil
	default:
		return Caller{}, ErrNoCallerIdentity
	}
}
