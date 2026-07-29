package filepreview

import (
	"bytes"
	_ "embed"
	"fmt"
	"html"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// reloadJS is the text shell's live-reload client: an EventSource on
// routeEvents that refetches the page and swaps the <pre> in place.
//
//go:embed assets/reload.js
var reloadJS []byte

// Reserved URL namespace. /__clank/ belongs to the overlay proxy and
// never reaches this server; /__file/ is ours — a project file literally
// named __file/… is shadowed, the same trade the proxy makes.
const (
	routePrefix = "/__file/"
	routeReload = routePrefix + "reload.js"
	routeEvents = routePrefix + "events"
)

// maxFileBytes caps files wrapped in the text shell (the whole file is
// escaped in memory). Bigger files 413 rather than balloon the process.
const maxFileBytes = 4 << 20

// nulSniffLen is how much of a file is scanned for NUL bytes to decide
// it's binary (a download beats a garbage text shell).
const nulSniffLen = 8 << 10

// Handler serves one project's files. It is the http.Handler behind
// Start; exported so tests (and the webpreview seam test) can drive it
// without a listener.
type Handler struct {
	root  *os.Root
	entry string
	mux   *http.ServeMux
	log   *log.Logger
}

// NewHandler validates Options and opens the project-root handle.
// Callers own Close (Start/Shutdown do it for them).
func NewHandler(opts Options) (*Handler, error) {
	if !filepath.IsAbs(opts.Root) {
		return nil, fmt.Errorf("filepreview: root must be absolute, got %q", opts.Root)
	}
	if opts.Entry == "" || strings.HasPrefix(opts.Entry, "/") {
		return nil, fmt.Errorf("filepreview: entry must be a relative path, got %q", opts.Entry)
	}
	lg := opts.Log
	if lg == nil {
		lg = log.Default()
	}
	root, err := os.OpenRoot(opts.Root)
	if err != nil {
		return nil, fmt.Errorf("filepreview: open root: %w", err)
	}
	h := &Handler{root: root, entry: opts.Entry, log: lg}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+routeReload, serveReloadJS)
	mux.HandleFunc("GET "+routeEvents, h.handleEvents)
	mux.HandleFunc("GET /{$}", h.handleIndex)
	mux.HandleFunc("GET /{path...}", h.handleFile)
	h.mux = mux
	return h, nil
}

// Close releases the project-root handle. The Handler must not serve
// after Close.
func (h *Handler) Close() {
	_ = h.root.Close()
}

// ServeHTTP gates every request on a loopback Host before routing.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !loopbackHost(r.Host) {
		// A foreign Host on a loopback listener is DNS rebinding: a
		// hostile page resolving its own domain to 127.0.0.1 to read
		// project files cross-origin. Real traffic (the browser, or the
		// overlay proxy's rewrite) always names loopback.
		http.Error(w, "forbidden host", http.StatusForbidden)
		return
	}
	h.mux.ServeHTTP(w, r)
}

// loopbackHost reports whether an HTTP Host header names this loopback
// server. The port is ignored — the overlay proxy forwards its own
// upstream port.
func loopbackHost(hostport string) bool {
	host := hostport
	if hp, _, err := net.SplitHostPort(hostport); err == nil {
		host = hp
	}
	host = strings.Trim(host, "[]")
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

func (h *Handler) handleIndex(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, (&url.URL{Path: "/" + h.entry}).String(), http.StatusFound)
}

// handleFile serves one project file the way the browser best renders
// it natively: .html as itself, browser-native binary types (images,
// video, pdf) as themselves, everything else in the text shell.
func (h *Handler) handleFile(w http.ResponseWriter, r *http.Request) {
	rel := r.PathValue("path")
	f, err := h.root.Open(rel)
	if err != nil {
		// Escapes (.. or symlinks out of root) and missing files alike:
		// nothing outside the project is acknowledged to exist.
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}

	if isRawHTML(rel) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.ServeContent(w, r, "", info.ModTime(), f)
		return
	}
	if ct := binaryMIMEType(rel); ct != "" {
		w.Header().Set("Content-Type", ct)
		http.ServeContent(w, r, "", info.ModTime(), f)
		return
	}
	h.serveTextShell(w, r, rel, f, info)
}

// isRawHTML: documents the browser renders as pages themselves — the
// overlay proxy injects straight into them, no shell needed.
func isRawHTML(rel string) bool {
	switch strings.ToLower(path.Ext(rel)) {
	case ".html", ".htm":
		return true
	}
	return false
}

// binaryMIMEPrefixes are content classes the browser handles natively
// (image viewer, video player, PDF viewer) — no text shell, no overlay.
var binaryMIMEPrefixes = []string{"image/", "video/", "audio/", "font/", "application/pdf"}

// binaryMIMEType returns the extension's mime type when it's a class
// the browser renders natively, else "".
func binaryMIMEType(rel string) string {
	ct := mime.TypeByExtension(strings.ToLower(path.Ext(rel)))
	for _, p := range binaryMIMEPrefixes {
		if strings.HasPrefix(ct, p) {
			return ct
		}
	}
	return ""
}

// serveTextShell renders any other file as escaped text in a minimal
// HTML page: visually the browser's native plain-text view (pre-wrap,
// like Chrome presents text/plain), but with a <head> for the overlay
// proxy to inject into and the live-reload client attached.
func (h *Handler) serveTextShell(w http.ResponseWriter, r *http.Request, rel string, f *os.File, info os.FileInfo) {
	if info.Size() > maxFileBytes {
		http.Error(w, fmt.Sprintf("file too large to preview (%d bytes, cap %d)", info.Size(), maxFileBytes), http.StatusRequestEntityTooLarge)
		return
	}
	data, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, "read: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if isBinary(data) {
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, "", info.ModTime(), bytes.NewReader(data))
		return
	}
	var b strings.Builder
	b.Grow(len(data) + 512)
	b.WriteString("<!doctype html>\n<html>\n<head>\n<meta charset=\"utf-8\">\n<title>")
	b.WriteString(html.EscapeString(rel))
	b.WriteString("</title>\n</head>\n<body>\n")
	b.WriteString(`<pre style="white-space: pre-wrap; word-wrap: break-word;">`)
	b.WriteString(html.EscapeString(string(data)))
	b.WriteString("</pre>\n<script src=\"" + routeReload + "\" defer></script>\n</body>\n</html>\n")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, b.String())
}

// isBinary: a NUL in the head of the file marks it non-text.
func isBinary(data []byte) bool {
	return bytes.IndexByte(data[:min(len(data), nulSniffLen)], 0) >= 0
}

func serveReloadJS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(reloadJS)
}
