package clankcli

import (
	"fmt"
	"net/url"
	"strings"
)

// clank://link is the deep-link a phone scans from the QR that
// `clank preview` renders. One scan re-points the phone's active gateway
// at the laptop and opens the preview. The QR carries no secret: a new
// phone authenticates through the typed-code pairing ceremony, a
// returning phone with its stored secret. The mobile app (clank-mobile
// app/scan.tsx) parses this exact shape — keep the scheme/host/param
// names in sync on both sides.
const (
	previewLinkScheme = "clank"
	previewLinkHost   = "link"

	// Bump previewLinkVersion when the param set changes in a way the
	// mobile parser must branch on. v carries it so an old app can reject
	// a newer link with a clear message instead of silently mis-parsing.
	previewLinkVersion = "1"

	previewLinkParamVersion    = "v"
	previewLinkParamGateway    = "gw"   // primary gateway base URL (tailnet if present, else LAN)
	previewLinkParamAlts       = "alt"  // comma-separated additional gateway base URLs (optional)
	previewLinkParamHostKey    = "hk"   // laptop's Ed25519 identity public key (public — verifies probes)
	previewLinkParamPreviewURL = "url"  // Metro dev server, e.g. http://192.168.1.20:8081
	previewLinkParamSessionID  = "sid"  // session to open in links produced by older clients (optional)
	previewLinkParamLocalPath  = "lp"   // laptop folder the agent + Metro run against
	previewLinkParamBackend    = "bk"   // agent backend the laptop can run (opencode|claude-code)
	previewLinkParamName       = "name" // laptop display name for the gateway picker (optional)
	previewLinkParamWorktreeID = "wid"  // preview key for /worktrees/<wid>/preview/{status,logs} polling
)

// PreviewLink is the payload carried by the QR.
//
//   - GatewayURL (+ Alts): where the phone reaches the daemon's bridge —
//     the candidate set it remembers and probes on every reconnect.
//   - HostKey: the laptop's identity public key. The phone pins it and
//     verifies every probe answer against it, so a remembered address
//     that got reassigned can't impersonate the laptop.
//   - PreviewURL: the dev server to open.
//   - SessionID: optional legacy field retained so older links remain readable.
//   - LocalPath + Backend: when there's no SessionID, the phone creates the
//     session itself on the first message — same call it makes for cloud
//     worktrees, with LocalPath instead of WorktreeID.
//   - WorktreeID: the preview key (folder slug) — lets the phone poll the
//     dev server's status/logs through the gateway while Metro boots,
//     instead of hitting a not-yet-listening port raw.
type PreviewLink struct {
	GatewayURL string
	Alts       []string
	HostKey    string
	PreviewURL string
	SessionID  string
	LocalPath  string
	Backend    string
	Name       string
	WorktreeID string
}

// Encode renders the link as a clank://link URL. GatewayURL and
// HostKey are required — a link that names no gateway, or one the
// phone couldn't verify a probe against, is a programmer error, not a
// partial-but-usable payload.
func (l PreviewLink) Encode() (string, error) {
	if l.GatewayURL == "" {
		return "", fmt.Errorf("preview link: GatewayURL is required")
	}
	if l.HostKey == "" {
		return "", fmt.Errorf("preview link: HostKey is required")
	}
	q := url.Values{}
	q.Set(previewLinkParamVersion, previewLinkVersion)
	q.Set(previewLinkParamGateway, l.GatewayURL)
	q.Set(previewLinkParamHostKey, l.HostKey)
	setIfPresent(q, previewLinkParamAlts, strings.Join(l.Alts, ","))
	setIfPresent(q, previewLinkParamPreviewURL, l.PreviewURL)
	setIfPresent(q, previewLinkParamSessionID, l.SessionID)
	setIfPresent(q, previewLinkParamLocalPath, l.LocalPath)
	setIfPresent(q, previewLinkParamBackend, l.Backend)
	setIfPresent(q, previewLinkParamName, l.Name)
	setIfPresent(q, previewLinkParamWorktreeID, l.WorktreeID)
	u := url.URL{Scheme: previewLinkScheme, Host: previewLinkHost, RawQuery: q.Encode()}
	return u.String(), nil
}

// ParsePreviewLink is the inverse of Encode. It exists mainly so a Go test
// can assert the round-trip the mobile parser must also satisfy.
func ParsePreviewLink(s string) (PreviewLink, error) {
	u, err := url.Parse(s)
	if err != nil {
		return PreviewLink{}, fmt.Errorf("parse preview link: %w", err)
	}
	if u.Scheme != previewLinkScheme || u.Host != previewLinkHost {
		return PreviewLink{}, fmt.Errorf("preview link: want %s://%s, got %q", previewLinkScheme, previewLinkHost, s)
	}
	q := u.Query()
	link := PreviewLink{
		GatewayURL: q.Get(previewLinkParamGateway),
		HostKey:    q.Get(previewLinkParamHostKey),
		PreviewURL: q.Get(previewLinkParamPreviewURL),
		SessionID:  q.Get(previewLinkParamSessionID),
		LocalPath:  q.Get(previewLinkParamLocalPath),
		Backend:    q.Get(previewLinkParamBackend),
		Name:       q.Get(previewLinkParamName),
		WorktreeID: q.Get(previewLinkParamWorktreeID),
	}
	if alts := q.Get(previewLinkParamAlts); alts != "" {
		link.Alts = strings.Split(alts, ",")
	}
	if link.GatewayURL == "" {
		return PreviewLink{}, fmt.Errorf("preview link: missing required %s in %q", previewLinkParamGateway, s)
	}
	if link.HostKey == "" {
		return PreviewLink{}, fmt.Errorf("preview link: missing required %s in %q", previewLinkParamHostKey, s)
	}
	return link, nil
}

func setIfPresent(q url.Values, key, value string) {
	if value != "" {
		q.Set(key, value)
	}
}
