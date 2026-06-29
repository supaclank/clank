package clankcli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os/user"
	"time"

	"github.com/acksell/clank/pkg/auth"
	"github.com/acksell/clank/pkg/blobstore"
	"github.com/acksell/clank/pkg/images"
)

// previewFrontDoor is the LAN-exposed, pairing-authenticated front door
// `clank preview` puts in front of the existing local daemon. It does NOT
// run its own gateway or host: it reverse-proxies every request to the
// daemon's unix socket — one gateway, one host.db, the same sessions and
// worktrees the TUI sees — except /v1/images, which it serves itself from
// a LAN blobstore so phone image uploads work without S3.
type previewFrontDoor struct {
	BaseURL string // http://<lanIP>:<port>
	Token   string // pairing bearer the QR carries
	UserID  string

	blob *blobstore.LAN
	srv  *http.Server
	ln   net.Listener
	log  *log.Logger
}

// startPreviewFrontDoor binds the LAN listener and proxies everything but
// /v1/images to the daemon at sockPath. advertiseIP is baked into the
// gateway + blob URLs; port 0 picks a free port.
func startPreviewFrontDoor(advertiseIP net.IP, port int, sockPath string, lg *log.Logger) (*previewFrontDoor, error) {
	if lg == nil {
		lg = log.Default()
	}
	token, err := randomToken(32)
	if err != nil {
		return nil, fmt.Errorf("generate pairing token: %w", err)
	}
	signKey := make([]byte, 32)
	if _, err := rand.Read(signKey); err != nil {
		return nil, fmt.Errorf("generate blob signing key: %w", err)
	}

	blob, err := blobstore.NewLAN("0.0.0.0:0", advertiseIP.String(), signKey)
	if err != nil {
		return nil, fmt.Errorf("start blob store: %w", err)
	}
	imagesSrv, err := images.NewServer(images.Config{Storage: blob})
	if err != nil {
		_ = blob.Close()
		return nil, fmt.Errorf("build images server: %w", err)
	}

	// Reverse-proxy everything else to the daemon's unix socket. The
	// daemon authenticates with AllowAll there (unix socket = trusted
	// local), so strip the phone's bearer before forwarding.
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = "daemon"
			pr.Out.Host = "daemon"
			pr.Out.Header.Del("Authorization")
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sockPath)
			},
		},
		FlushInterval: -1, // stream SSE (/events) instead of buffering
	}

	userID := previewUserID()
	inner := http.NewServeMux()
	inner.Handle("/v1/images", imagesSrv.Handler())
	inner.Handle("/", proxy)
	handler := auth.Middleware(inner, &auth.StaticBearer{Token: token, UserID: userID})

	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		_ = blob.Close()
		return nil, fmt.Errorf("listen on port %d: %w", port, err)
	}
	fd := &previewFrontDoor{
		BaseURL: fmt.Sprintf("http://%s:%d", advertiseIP, ln.Addr().(*net.TCPAddr).Port),
		Token:   token,
		UserID:  userID,
		blob:    blob,
		srv: &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
			// WriteTimeout omitted: FlushInterval: -1 keeps SSE connections open indefinitely.
			// ReadHeaderTimeout (not ReadTimeout) avoids cutting image upload body reads.
		},
		ln:  ln,
		log: lg,
	}
	go func() {
		if err := fd.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			lg.Printf("preview front door serve: %v", err)
		}
	}()
	return fd, nil
}

// Shutdown stops the LAN listener and the blob store. It never touches the
// daemon — its lifecycle is the caller's concern (stop only if we started it).
func (fd *previewFrontDoor) Shutdown(ctx context.Context) {
	if err := fd.srv.Shutdown(ctx); err != nil {
		fd.log.Printf("preview front door shutdown: %v", err)
	}
	_ = fd.blob.Close()
}

// previewUserID is the identity the pairing bearer resolves to at the LAN
// boundary. The OS username keeps session attribution sensible.
func previewUserID() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "local"
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
