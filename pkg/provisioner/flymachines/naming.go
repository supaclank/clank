package flymachines

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Fixed names for the one machine and one volume inside each per-user
// app. The app is the namespace; nothing inside it needs a unique name.
const (
	machineName = "clank-host"
	volumeName  = "data"
)

// appNameFor derives the user's Fly app name: <prefix>-<12 hex of
// sha256(userID)>. Fly app names are GLOBALLY unique across all of
// Fly and length-limited, and userIDs are free-form (Supabase subs
// are 36-char UUIDs), so a hash beats sanitized-userID embedding:
// deterministic, always valid, and leaks nothing about the tenant in
// a name visible to anyone who can query DNS certs.
//
// The requested name can still lose a global-uniqueness race against
// an app outside this org; ensureApp surfaces that as an error asking
// the operator to change AppNamePrefix. Once created, ExternalID in
// the store row is the source of truth for the actual name — never
// re-derive.
func appNameFor(prefix, userID string) string {
	sum := sha256.Sum256([]byte(userID))
	return prefix + "-" + hex.EncodeToString(sum[:6])
}

// networkNameFor names the per-tenant private network. Derived from
// the app name so a recreated app lands back on the same network.
func networkNameFor(appName string) string {
	return appName + "-net"
}

// hostnameFor is the stable identifier surfaced to upper layers
// (session metadata, hub catalog) — matches the other providers'
// "<provider>-<suffix>" convention.
func hostnameFor(appName string) string {
	suffix := appName
	if i := strings.LastIndex(appName, "-"); i >= 0 {
		suffix = appName[i+1:]
	}
	if len(suffix) > 12 {
		suffix = suffix[:12]
	}
	return "flym-" + suffix
}
