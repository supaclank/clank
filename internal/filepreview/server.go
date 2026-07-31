// Package filepreview serves one project's files raw over loopback HTTP
// so `clank preview <file>` can open them in the browser. It is the
// upstream behind the overlay-injecting proxy (internal/webpreview):
// text files come wrapped in a minimal HTML shell so the proxy has a
// <head> to inject the clank overlay into, and shell pages live-reload
// via an SSE watch — that's how you keep the document in front of you
// while the agent edits it on disk.
package filepreview

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"
)

// Options configures Start.
type Options struct {
	// Root is the absolute project directory. Nothing outside it is
	// reachable (os.Root containment, symlink escapes included).
	Root string

	// Entry is the slash-relative path "/" redirects to — the file the
	// user asked to preview.
	Entry string

	Log *log.Logger
}

// Server is a running file-preview server.
type Server struct {
	// URL is the browser-facing address, http://127.0.0.1:<port>.
	URL string

	// Port is the listener's TCP port — the overlay proxy's upstream
	// (webpreview.Options.UpstreamPort).
	Port int

	srv     *http.Server
	handler *Handler
	cancel  context.CancelFunc
	log     *log.Logger
}

// Start binds a loopback listener and serves in the background.
// Loopback-only on purpose: the browser and the overlay proxy are the
// only peers, and project files must stay off the network.
func Start(opts Options) (*Server, error) {
	h, err := NewHandler(opts)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		h.Close()
		return nil, fmt.Errorf("filepreview: listen: %w", err)
	}
	// Request contexts derive from baseCtx so Shutdown can end the
	// held-open SSE streams instead of waiting them out.
	baseCtx, cancel := context.WithCancel(context.Background())
	port := ln.Addr().(*net.TCPAddr).Port
	s := &Server{
		URL:     fmt.Sprintf("http://127.0.0.1:%d", port),
		Port:    port,
		handler: h,
		cancel:  cancel,
		log:     h.log,
		srv: &http.Server{
			Handler:           h,
			BaseContext:       func(net.Listener) context.Context { return baseCtx },
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
			// No Read/WriteTimeout: /__file/events is a held-open SSE stream.
		},
	}
	go func() {
		if serr := s.srv.Serve(ln); serr != nil && serr != http.ErrServerClosed {
			s.log.Printf("filepreview: serve: %v", serr)
		}
	}()
	return s, nil
}

// Shutdown ends the SSE streams (base-context cancel), stops the
// listener, and releases the project-root handle.
func (s *Server) Shutdown(ctx context.Context) {
	s.cancel()
	if err := s.srv.Shutdown(ctx); err != nil {
		s.log.Printf("filepreview: shutdown: %v", err)
	}
	s.handler.Close()
}
