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
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
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
	// UpstreamPort is the dev server (Vite) the proxy fronts, on
	// 127.0.0.1. Required.
	UpstreamPort int

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

	// Engine powers dictation; nil marks voice unavailable in the
	// overlay config and 503s the voice endpoint.
	Engine Engine

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
	if opts.UpstreamPort == 0 {
		return nil, fmt.Errorf("webpreview: upstream port is required")
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

	cfg := map[string]any{}
	for k, v := range opts.OverlayConfig {
		cfg[k] = v
	}
	cfg["token"] = opts.Token
	cfg["voice"] = opts.Engine != nil
	cfgJSON, err := json.Marshal(cfg) // encoding/json escapes <,>,& — safe inside <script>
	if err != nil {
		return nil, fmt.Errorf("webpreview: marshal overlay config: %w", err)
	}
	snippet := []byte("<script>window.__CLANK_PREVIEW = " + string(cfgJSON) + ";</script>\n" +
		`<script type="module" src="/__clank/overlay.js"></script>`)

	upstream := newUpstreamProxy(opts.UpstreamPort, snippet, lg)
	daemon := newDaemonProxy(opts.DaemonSocketPath)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /__clank/overlay.js", serveJS(overlayJS))
	mux.HandleFunc("GET /__clank/worklet.js", serveJS(workletJS))
	mux.Handle("/__clank/api/", requireToken(opts.Token,
		http.StripPrefix("/__clank/api", daemon)))
	mux.Handle("GET /__clank/voice", requireToken(opts.Token,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if opts.Engine == nil {
				http.Error(w, "no dictation engine configured (set "+EngineEnvVar+")", http.StatusServiceUnavailable)
				return
			}
			serveVoiceWS(w, r, opts.Engine, lg)
		})))
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
// guards on 200+text/html so 101s are untouched.
func newUpstreamProxy(port int, snippet []byte, lg *log.Logger) *httputil.ReverseProxy {
	target := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", port)}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
			// Ask for identity encoding so the HTML rewrite below never
			// has to gunzip. Vite dev serves uncompressed anyway; this
			// pins it for frameworks that don't.
			pr.Out.Header.Set("Accept-Encoding", "identity")
		},
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode != http.StatusOK {
				return nil
			}
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
				return nil
			}
			// The Rewrite hook above requests identity encoding, but an
			// upstream that ignores Accept-Encoding would otherwise get its
			// compressed bytes searched and corrupted by injectHTML.
			if ce := resp.Header.Get("Content-Encoding"); ce != "" && ce != "identity" {
				return nil
			}
			body, overflow, err := readUpTo(resp.Body, maxInjectHTMLBytes)
			if err != nil {
				return err
			}
			if overflow != nil {
				// Improbably large HTML: stream it through untouched.
				resp.Body = overflow
				return nil
			}
			injected := injectHTML(body, snippet)
			resp.Body = io.NopCloser(bytes.NewReader(injected))
			resp.ContentLength = int64(len(injected))
			resp.Header.Set("Content-Length", strconv.Itoa(len(injected)))
			// A dev-mode CSP (e.g. SvelteKit kit.csp) would block the
			// injected inline config script; the overlay is a dev tool on
			// a loopback origin, so drop it for injected pages only.
			resp.Header.Del("Content-Security-Policy")
			resp.Header.Del("Content-Security-Policy-Report-Only")
			// The injected config embeds this run's token. A cached copy
			// (or a 304 revalidation of one) would leave the page holding
			// a dead token after a preview restart — every /__clank call
			// 401s with no visible error. Never let injected HTML cache.
			resp.Header.Set("Cache-Control", "no-store")
			resp.Header.Del("ETag")
			resp.Header.Del("Last-Modified")
			return nil
		},
		FlushInterval: -1, // stream non-HTML passthrough (SSE-style dev endpoints)
		ErrorLog:      lg,
	}
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
		if subtle.ConstantTimeCompare([]byte(got), want) != 1 {
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
