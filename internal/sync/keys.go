package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// RepoKey returns the stable key used by the sync protocol to identify a
// repo. It is the hex SHA-256 of the canonicalized remote URL.
//
// Note: this is intentionally per-repo (not per-branch) — the cloud-hub
// mirror is one bare repo per RemoteURL. The branch travels separately
// in the X-Clank-Branch header. Sync does NOT use agent.RepoKey directly
// because that includes the branch and contains characters (NUL, slashes)
// that don't fit in a URL path.
func RepoKey(remoteURL string) string {
	canon := strings.TrimSpace(remoteURL)
	if canon == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(canon))
	return hex.EncodeToString(sum[:])
}
