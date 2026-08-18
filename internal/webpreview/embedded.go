package webpreview

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const (
	OverlayPath       = "/__clank/overlay.js"
	ChatPath          = "/__clank/chat.js"
	MarkdownPath      = "/__clank/markdown.js"
	TranscriptPath    = "/__clank/transcript.js"
	SettingsPath      = "/__clank/settings.js"
	SourceControlPath = "/__clank/sourcecontrol.js"
	BoxPosPath        = "/__clank/boxpos.js"
	LauncherPath      = "/__clank/launcher.js"
	WorkletPath       = "/__clank/worklet.js"
	APIPrefix         = "/__clank/api"

	// NativePreviewUserAgentToken keeps the JS overlay out of clank-mobile's
	// WebView, where the Kotlin prompt box owns the interaction surface.
	NativePreviewUserAgentToken = "ClankNativePreview/1"
)

// ShouldInjectOverlay reports whether a preview page needs the browser UI.
func ShouldInjectOverlay(userAgent string) bool {
	return !strings.Contains(userAgent, NativePreviewUserAgentToken)
}

// OverlaySnippet renders the config and module tags injected into preview HTML.
func OverlaySnippet(config map[string]any) ([]byte, error) {
	cfgJSON, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("webpreview: marshal overlay config: %w", err)
	}
	return []byte("<script>window.__CLANK_PREVIEW = " + string(cfgJSON) + ";</script>\n" +
		`<script type="module" src="` + OverlayPath + `"></script>`), nil
}

// ServeOverlayAsset serves one embedded overlay module and reports whether the
// request matched a reserved asset path.
func ServeOverlayAsset(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	var body []byte
	switch r.URL.Path {
	case OverlayPath:
		body = overlayJS
	case ChatPath:
		body = chatJS
	case MarkdownPath:
		body = markdownJS
	case TranscriptPath:
		body = transcriptJS
	case SettingsPath:
		body = settingsJS
	case SourceControlPath:
		body = sourceControlJS
	case BoxPosPath:
		body = boxPosJS
	case LauncherPath:
		body = launcherJS
	case WorkletPath:
		body = workletJS
	default:
		return false
	}
	serveJS(body).ServeHTTP(w, r)
	return true
}

// InjectOverlayResponse rewrites a successful identity-encoded HTML response.
// Non-HTML, compressed, and oversized bodies pass through unchanged.
func InjectOverlayResponse(resp *http.Response, snippet []byte) error {
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		return nil
	}
	if ce := resp.Header.Get("Content-Encoding"); ce != "" && ce != "identity" {
		return nil
	}
	body, overflow, err := readUpTo(resp.Body, maxInjectHTMLBytes)
	if err != nil {
		return err
	}
	if overflow != nil {
		resp.Body = overflow
		return nil
	}
	injected := injectHTML(body, snippet)
	resp.Body = io.NopCloser(bytes.NewReader(injected))
	resp.ContentLength = int64(len(injected))
	resp.Header.Set("Content-Length", strconv.Itoa(len(injected)))
	// TODO(ai-review): strips CSP unconditionally, weakening the previewed
	// app's own XSS defenses on public/shareable hosted previews; consider
	// nonce-based injection instead. https://github.com/supaclank/clank/pull/216#discussion_r3699214068
	resp.Header.Del("Content-Security-Policy")
	resp.Header.Del("Content-Security-Policy-Report-Only")
	resp.Header.Set("Cache-Control", "no-store")
	resp.Header.Del("ETag")
	resp.Header.Del("Last-Modified")
	return nil
}
