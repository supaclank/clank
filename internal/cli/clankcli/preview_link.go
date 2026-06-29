package clankcli

import (
	"fmt"
	"net/url"
)

// clank://link is the deep-link a phone scans from the QR that
// `clank preview` renders. One scan re-points the phone's active gateway
// at the laptop, authenticates it with the pairing token, and opens the
// preview. The mobile app (clank-mobile app/scan.tsx) parses this exact
// shape — keep the scheme/host/param names in sync on both sides.
const (
	previewLinkScheme = "clank"
	previewLinkHost   = "link"

	// Bump previewLinkVersion when the param set changes in a way the
	// mobile parser must branch on. v carries it so an old app can reject
	// a newer link with a clear message instead of silently mis-parsing.
	previewLinkVersion = "1"

	previewLinkParamVersion    = "v"
	previewLinkParamGateway    = "gw"   // gateway base URL, e.g. http://192.168.1.20:7878
	previewLinkParamToken      = "tok"  // pairing bearer (auth.StaticBearer)
	previewLinkParamPreviewURL = "url"  // Metro dev server, e.g. http://192.168.1.20:8081
	previewLinkParamSessionID  = "sid"  // a prompt-started session to open (optional)
	previewLinkParamLocalPath  = "lp"   // laptop folder the agent + Metro run against
	previewLinkParamBackend    = "bk"   // agent backend the laptop can run (opencode|claude-code)
	previewLinkParamName       = "name" // project display name (optional)
)

// PreviewLink is the payload carried by the QR.
//
//   - GatewayURL + Token: the pairing capability (required).
//   - PreviewURL: the dev server to open.
//   - SessionID: set only when `clank preview <prompt>` already started an
//     agent — the phone attaches to it.
//   - LocalPath + Backend: when there's no SessionID, the phone creates the
//     session itself on the first message — same call it makes for cloud
//     worktrees, with LocalPath instead of WorktreeID.
type PreviewLink struct {
	GatewayURL string
	Token      string
	PreviewURL string
	SessionID  string
	LocalPath  string
	Backend    string
	Name       string
}

// Encode renders the link as a clank://link URL. GatewayURL and Token are
// required — they're the whole point of pairing, so a link without them is
// a programmer error, not a partial-but-usable payload.
func (l PreviewLink) Encode() (string, error) {
	if l.GatewayURL == "" {
		return "", fmt.Errorf("preview link: GatewayURL is required")
	}
	if l.Token == "" {
		return "", fmt.Errorf("preview link: Token is required")
	}
	q := url.Values{}
	q.Set(previewLinkParamVersion, previewLinkVersion)
	q.Set(previewLinkParamGateway, l.GatewayURL)
	q.Set(previewLinkParamToken, l.Token)
	setIfPresent(q, previewLinkParamPreviewURL, l.PreviewURL)
	setIfPresent(q, previewLinkParamSessionID, l.SessionID)
	setIfPresent(q, previewLinkParamLocalPath, l.LocalPath)
	setIfPresent(q, previewLinkParamBackend, l.Backend)
	setIfPresent(q, previewLinkParamName, l.Name)
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
		Token:      q.Get(previewLinkParamToken),
		PreviewURL: q.Get(previewLinkParamPreviewURL),
		SessionID:  q.Get(previewLinkParamSessionID),
		LocalPath:  q.Get(previewLinkParamLocalPath),
		Backend:    q.Get(previewLinkParamBackend),
		Name:       q.Get(previewLinkParamName),
	}
	if link.GatewayURL == "" || link.Token == "" {
		return PreviewLink{}, fmt.Errorf("preview link: missing required %s/%s in %q", previewLinkParamGateway, previewLinkParamToken, s)
	}
	return link, nil
}

func setIfPresent(q url.Values, key, value string) {
	if value != "" {
		q.Set(key, value)
	}
}
