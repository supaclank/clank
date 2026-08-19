package webpreview

import (
	"bytes"
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// overlayJS is the guest-side overlay client (shadow-DOM UI, element
// inspector, session client, push-to-talk). Embedded like the Metro
// shim in internal/host/preview/inject.go — plain JS, no build step,
// nothing written into the user's repo.
//
//go:embed overlay/overlay.js
var overlayJS []byte

// chatJS is the overlay's pure chat-protocol module (question cards,
// permission queue, transcript reconcile) — a separate ES module so
// `node --test` covers it without a DOM; overlay.js imports it.
//
//go:embed overlay/chat.js
var chatJS []byte

// markdownJS projects chat Markdown into a DOM-safe block/token model.
//
//go:embed overlay/markdown.js
var markdownJS []byte

// transcriptJS renders structured transcript rows into the overlay shadow DOM.
//
//go:embed overlay/transcript.js
var transcriptJS []byte

// settingsJS is the overlay's pure agent-profile selection module.
//
//go:embed overlay/settings.js
var settingsJS []byte

// sourceControlJS is the overlay's pure source-control module (remote
// state presentation, PR request shapes, agent hand-off prompts).
//
//go:embed overlay/sourcecontrol.js
var sourceControlJS []byte

// boxPosJS is the overlay's pure box-position module (viewport-resize
// clamping of the drag offset).
//
//go:embed overlay/boxpos.js
var boxPosJS []byte

// launcherJS owns the overlay launcher's pure presentation state.
//
//go:embed overlay/launcher.js
var launcherJS []byte

// resizeJS is the overlay's pure box-resize module (top-edge drag
// arithmetic, composer autosize bounds).
//
//go:embed overlay/resize.js
var resizeJS []byte

// workletJS is the AudioWorklet processor that batches mic PCM for the
// dictation WebSocket. Served as its own module because AudioWorklets
// load from a URL, not inline.
//
//go:embed overlay/worklet.js
var workletJS []byte

// maxInjectHTMLBytes caps how much of an HTML response we buffer for
// injection. Dev-server HTML is kilobytes; anything past this cap
// streams through untouched (no overlay on that page) rather than
// ballooning memory.
const maxInjectHTMLBytes = 8 << 20

// Options configures Start.
type Options struct {
	// UpstreamURL is the HTTP(S) origin the proxy fronts. Required.
	UpstreamURL *url.URL

	// DaemonSocketPath is the clank daemon's unix socket; /__clank/api/*
	// relays there. Required.
	DaemonSocketPath string

	// Token gates /__clank/api/* and /__clank/voice. The injected page
	// config carries it; nothing else on the machine learns it. This is
	// the same trust move as the LAN front door's pairing token, scoped
	// down to loopback.
	Token string

	// OverlayConfig is serialized into window.__CLANK_PREVIEW for the
	// overlay (session context: local_path, backend, hostname, name,
	// optional session_id). Token and voice availability are added here.
	OverlayConfig map[string]any

	// Engine powers local dictation; nil marks the local engine
	// unavailable in the overlay config and 503s the voice endpoint.
	// The overlay may still offer the browser's Web Speech API, which
	// it detects client-side.
	Engine Engine

	// DictationEngine is the persisted engine choice injected into the
	// overlay config. Empty means unchosen (the overlay asks on first
	// dictation); anything else must parse via ParseDictationEngine.
	DictationEngine DictationEngine

	// PersistDictationEngine stores a choice made in the overlay's
	// engine picker so it survives preview restarts. nil means choices
	// only last for this run.
	PersistDictationEngine func(DictationEngine) error

	// LauncherSeen suppresses the first-use coachmark. PersistLauncherSeen
	// stores the acknowledgement across auto-assigned preview ports.
	LauncherSeen        bool
	PersistLauncherSeen func() error

	// ListenPort for the proxy on 127.0.0.1; 0 picks a free port.
	ListenPort int

	Log *log.Logger
}

// Server is a running overlay proxy.
type Server struct {
	// URL is the browser-facing address, http://127.0.0.1:<port>.
	URL string

	srv *http.Server
	ln  net.Listener
	log *log.Logger
}

// Start binds the loopback listener and serves in the background.
// Loopback-only on purpose: unlike the phone flow there's no LAN peer,
// and not binding 0.0.0.0 keeps the daemon relay off the network.
func Start(opts Options) (*Server, error) {
	target, err := validatedUpstreamURL(opts.UpstreamURL)
	if err != nil {
		return nil, err
	}
	if opts.DaemonSocketPath == "" {
		return nil, fmt.Errorf("webpreview: daemon socket path is required")
	}
	if opts.Token == "" {
		return nil, fmt.Errorf("webpreview: token is required")
	}
	lg := opts.Log
	if lg == nil {
		lg = log.Default()
	}

	if opts.DictationEngine != "" {
		if _, ok := ParseDictationEngine(string(opts.DictationEngine)); !ok {
			return nil, fmt.Errorf("webpreview: unknown dictation engine %q", opts.DictationEngine)
		}
	}

	cfg := map[string]any{}
	maps.Copy(cfg, opts.OverlayConfig)
	cfg["token"] = opts.Token
	// Local engine availability only — Web Speech availability is a
	// browser fact the overlay detects itself. The kind lets the picker
	// name what "fully local" actually runs (Parakeet vs. a user command).
	cfg["voice"] = opts.Engine != nil
	cfg["voice_engine"] = localVoiceKind(opts.Engine)
	if _, err := json.Marshal(cfg); err != nil { // validate caller values once, at Start
		return nil, fmt.Errorf("webpreview: marshal overlay config: %w", err)
	}
	dictation := &dictationState{engine: opts.DictationEngine}
	launcher := newLauncherState(opts.LauncherSeen)
	// Built per response, not once: the engine picker changes state
	// mid-run and a reloaded page must see the current choice.
	snippet := func() []byte {
		m := make(map[string]any, len(cfg)+1)
		maps.Copy(m, cfg)
		m["dictation_engine"] = string(dictation.get())
		m["launcher_seen"] = launcher.isSeen()
		cfgJSON, _ := json.Marshal(m) // encoding/json escapes <,>,& — safe inside <script>; validated above
		return []byte("<script>window.__CLANK_PREVIEW = " + string(cfgJSON) + ";</script>\n" +
			`<script type="module" src="` + OverlayPath + `"></script>`)
	}

	upstream := newUpstreamProxy(target, snippet, lg)
	daemon := newDaemonProxy(opts.DaemonSocketPath)

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+OverlayPath, serveJS(overlayJS))
	mux.HandleFunc("GET "+ChatPath, serveJS(chatJS))
	mux.HandleFunc("GET "+MarkdownPath, serveJS(markdownJS))
	mux.HandleFunc("GET "+TranscriptPath, serveJS(transcriptJS))
	mux.HandleFunc("GET "+SettingsPath, serveJS(settingsJS))
	mux.HandleFunc("GET "+SourceControlPath, serveJS(sourceControlJS))
	mux.HandleFunc("GET "+BoxPosPath, serveJS(boxPosJS))
	mux.HandleFunc("GET "+LauncherPath, serveJS(launcherJS))
	mux.HandleFunc("GET "+ResizePath, serveJS(resizeJS))
	mux.HandleFunc("GET "+WorkletPath, serveJS(workletJS))
	mux.Handle(APIPrefix+"/", requireToken(opts.Token,
		http.StripPrefix(APIPrefix, daemon)))
	mux.Handle("GET /__clank/voice", requireToken(opts.Token,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if opts.Engine == nil {
				http.Error(w, "no dictation engine configured (set "+EngineEnvVar+")", http.StatusServiceUnavailable)
				return
			}
			serveVoiceWS(w, r, opts.Engine, lg)
		})))
	mux.Handle("POST /__clank/voice/engine", requireToken(opts.Token,
		handleSetDictationEngine(dictation, opts.PersistDictationEngine, lg)))
	mux.Handle("POST "+LauncherSeenPath, requireToken(opts.Token,
		handleLauncherSeen(launcher, opts.PersistLauncherSeen, lg)))
	mux.Handle("/", upstream)

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", opts.ListenPort))
	if err != nil {
		return nil, fmt.Errorf("webpreview: listen: %w", err)
	}
	s := &Server{
		URL: fmt.Sprintf("http://127.0.0.1:%d", ln.Addr().(*net.TCPAddr).Port),
		ln:  ln,
		log: lg,
		srv: &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
			// No Read/WriteTimeout: the daemon relay carries SSE and the
			// voice socket is held for the whole preview session.
		},
	}
	go func() {
		if serr := s.srv.Serve(ln); serr != nil && serr != http.ErrServerClosed {
			lg.Printf("webpreview: serve: %v", serr)
		}
	}()
	return s, nil
}

// Shutdown stops the listener. The dev server and daemon are the
// caller's concern, same split as the LAN front door.
func (s *Server) Shutdown(ctx context.Context) {
	if err := s.srv.Shutdown(ctx); err != nil {
		s.log.Printf("webpreview: shutdown: %v", err)
	}
}

// newUpstreamProxy fronts the dev server and injects the overlay into
// HTML responses. WebSocket upgrades (Vite HMR) pass through
// httputil.ReverseProxy's native upgrade handling; ModifyResponse
// guards on 200+text/html so 101s are untouched. snippet is invoked
// per injected response (its config reflects live state).
func newUpstreamProxy(target *url.URL, snippet func() []byte, lg *log.Logger) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			inboundHost := pr.In.Host
			pr.SetURL(target)
			pr.Out.Host = target.Host
			rewriteBrowserOrigin(pr.Out, inboundHost, target)
			// Ask for identity encoding so the HTML rewrite below never
			// has to gunzip. Vite dev serves uncompressed anyway; this
			// pins it for frameworks that don't.
			pr.Out.Header.Set("Accept-Encoding", "identity")
		},
		ModifyResponse: func(resp *http.Response) error {
			return InjectOverlayResponse(resp, snippet())
		},
		FlushInterval: -1, // stream non-HTML passthrough (SSE-style dev endpoints)
		ErrorLog:      lg,
	}
}

func validatedUpstreamURL(target *url.URL) (*url.URL, error) {
	if target == nil {
		return nil, fmt.Errorf("webpreview: upstream URL is required")
	}
	if !target.IsAbs() || target.Host == "" {
		return nil, fmt.Errorf("webpreview: upstream URL must be absolute")
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return nil, fmt.Errorf("webpreview: upstream URL must use http or https")
	}
	// Injected pages receive the daemon token. Never inject it into remote content.
	if !isLoopbackHost(target.Hostname()) {
		return nil, fmt.Errorf("webpreview: upstream URL must use a loopback host")
	}
	if target.User != nil || target.RawQuery != "" || target.ForceQuery || target.Fragment != "" || (target.Path != "" && target.Path != "/") {
		return nil, fmt.Errorf("webpreview: upstream URL must be an origin only")
	}
	clone := *target
	clone.Path = ""
	clone.RawPath = ""
	return &clone, nil
}

func isLoopbackHost(hostname string) bool {
	if strings.EqualFold(hostname, "localhost") {
		return true
	}
	ip := net.ParseIP(hostname)
	return ip != nil && ip.IsLoopback()
}

// newDaemonProxy relays to the daemon's unix socket — the same rewrite
// the LAN front door uses: scheme/host pinned to the socket dial, any
// browser-supplied Authorization stripped (the daemon's socket listener
// authenticates as AllowAll; the token check happened in requireToken).
func newDaemonProxy(socketPath string) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = "daemon"
			pr.Out.Host = "daemon"
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("X-Clank-Token")
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
		FlushInterval: -1, // /events is SSE
	}
}

// requireToken admits requests carrying the per-run token as
// `Authorization: Bearer <t>`, `X-Clank-Token: <t>`, or — for the
// WebSocket path, which can't set headers from a browser — `?t=<t>`.
func requireToken(token string, next http.Handler) http.Handler {
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-Clank-Token")
		if got == "" {
			if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
				got = strings.TrimPrefix(h, "Bearer ")
			}
		}
		if got == "" {
			got = r.URL.Query().Get("t")
		}
		// Length check first: cheap (no allocation), and rejects an
		// oversized got before []byte(got) copies it — a local client
		// can't force a large per-request allocation just by sending a
		// long garbage token.
		if len(got) != len(want) || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func serveJS(body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(body)
	}
}

// readUpTo reads r fully when it fits in limit, returning (body, nil,
// nil). When it doesn't fit, it returns (nil, replacement, nil): a
// ReadCloser stitching the consumed prefix back onto the remainder so
// the caller can stream the response through unmodified.
func readUpTo(rc io.ReadCloser, limit int) ([]byte, io.ReadCloser, error) {
	var buf bytes.Buffer
	_, err := io.CopyN(&buf, rc, int64(limit)+1)
	if err == io.EOF {
		rc.Close()
		return buf.Bytes(), nil, nil
	}
	if err != nil {
		rc.Close()
		return nil, nil, err
	}
	// n == limit+1 here (CopyN only returns a nil error after copying
	// exactly that many bytes): the body overflowed the cap. Keep rc
	// open — it's stitched into the replacement reader below, and the
	// caller's eventual Close on that reader closes rc in turn.
	return nil, struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(buf.Bytes()), rc), rc}, nil
}
