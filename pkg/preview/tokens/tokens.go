// Package tokens holds the constants and helpers shared between
// clank-host (which receives a token in the webhook response and
// returns it to mobile) and clankgw (which mints tokens, resolves
// them on subdomain requests, and gates visibility).
//
// Keeping these in one package per CLAUDE.md "no magic strings": the
// visibility enum, hostname prefix, default TTL, and URL builder all
// live in one place so a typo on either side surfaces at compile time.
package tokens

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Visibility is the auth policy a preview URL carries.
//
//   - VisibilityOwnerOnly — gateway requires a JWT whose `sub` matches
//     the route's owner_user_id. Default for new tokens.
//   - VisibilityPublic — gateway accepts anonymous requests; anyone
//     with the URL gets the preview. Used for shareable links.
type Visibility string

const (
	VisibilityOwnerOnly Visibility = "owner_only"
	VisibilityPublic    Visibility = "public"
)

// Valid reports whether v is a recognized visibility. The DB CHECK
// constraint enforces the same set; this is the Go-side guard so a
// bad value never reaches the query layer.
func (v Visibility) Valid() bool {
	switch v {
	case VisibilityOwnerOnly, VisibilityPublic:
		return true
	}
	return false
}

const (
	// DefaultServiceName is the service_name registered for the implicit
	// Expo launch. Configured web launches register their declared names.
	DefaultServiceName = "default"

	// DefaultTokenTTL is how long a freshly minted token stays valid
	// before requiring re-registration. 24h balances "the user comes
	// back to the same URL the next day" against "stale rows don't
	// pile up forever."
	DefaultTokenTTL = 24 * time.Hour

	// TokenBytes is the entropy in a fresh token. 16 bytes (128 bits)
	// is enough that an attacker enumerating preview-<token>.* via DNS
	// hits the cosmic background radiation rate before a hit.
	TokenBytes = 16

	// HostPrefix is the leftmost-label prefix on every preview URL.
	// Picked so the wildcard match `preview-*.<root>` is unambiguous
	// and doesn't collide with any other subdomain pattern. Constant
	// shared with the gateway's subdomain matcher.
	HostPrefix = "preview-"
)

// ErrInvalidVisibility is returned by SetVisibility callers when v
// isn't a recognized enum value. The DB CHECK would catch this too,
// but failing fast at the Go boundary avoids a confusing 500.
var ErrInvalidVisibility = errors.New("preview: invalid visibility")

// New mints a fresh URL-safe token. The byte stream is base32-encoded
// (no padding, lowercase) so the result is a single DNS label that
// works as the leftmost component of preview-<token>.<root>.
//
// 16 bytes → 26 chars. Lowercase a-z + 2-7. Safe in any DNS label.
func New() (string, error) {
	var b [TokenBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("preview tokens: read random: %w", err)
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])), nil
}

// HostFor returns the public host portion of a preview URL —
// "preview-<token>.<rootDomain>". No scheme. rootDomain is the
// wildcard zone the gateway was configured with at boot
// (e.g. "clankexample.dev").
func HostFor(token, rootDomain string) string {
	return HostPrefix + token + "." + rootDomain
}

// ParseHost extracts the token from a Host header of the form
// "preview-<token>.<rootDomain>[:port]" against the configured root
// domain. Returns (token, true) on match, (_, false) otherwise.
//
// Strips an optional ":port" suffix (browsers send `Host: foo:443`
// only for non-standard ports, but be defensive). Returns false for
// a bare "preview-" with no token or a token containing a dot
// (which would otherwise let a malformed token escape into a
// neighboring wildcard zone).
func ParseHost(host, rootDomain string) (string, bool) {
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	suffix := "." + rootDomain
	if !strings.HasSuffix(host, suffix) {
		return "", false
	}
	leftmost := strings.TrimSuffix(host, suffix)
	if !strings.HasPrefix(leftmost, HostPrefix) {
		return "", false
	}
	token := strings.TrimPrefix(leftmost, HostPrefix)
	if token == "" || strings.Contains(token, ".") {
		return "", false
	}
	return token, true
}

// URLFor returns the canonical URL a client uses to reach the
// preview. scheme + port mirror the API request that minted the
// route — so cloud deploys get https + no port, and local docker
// dev gets http + the gateway's listen port. The path the caller
// wants is appended to "/" so the returned URL is a valid base.
//
// scheme defaults to "https" when empty (callers without request
// context can still use the default cloud shape).
func URLFor(token, rootDomain, scheme, port string) string {
	if scheme == "" {
		scheme = "https"
	}
	host := HostFor(token, rootDomain)
	if port != "" {
		host = host + ":" + port
	}
	u := url.URL{Scheme: scheme, Host: host, Path: "/"}
	return u.String()
}
