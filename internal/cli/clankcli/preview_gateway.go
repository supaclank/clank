package clankcli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/user"

	"github.com/acksell/clank/pkg/auth"
	"github.com/acksell/clank/pkg/blobstore"
	"github.com/acksell/clank/pkg/gateway"
	"github.com/acksell/clank/pkg/images"
	localprov "github.com/acksell/clank/pkg/provisioner/local"
)

// previewGateway is the ephemeral, LAN-exposed gateway `clank preview`
// boots for the phone: its own clank-host (isolated --data-dir),
// pairing-token auth, and a LAN blobstore so phone image uploads work
// without S3. It is fully torn down by Shutdown and never touches the
// shared unix-socket daemon the TUI uses.
type previewGateway struct {
	BaseURL string // http://<lanIP>:<port>
	Token   string // pairing bearer the QR carries
	UserID  string

	prov *localprov.Provisioner
	blob *blobstore.LAN
	srv  *http.Server
	ln   net.Listener
	log  *log.Logger
}

// startPreviewGateway builds and serves the ephemeral gateway. advertiseIP
// is the LAN address baked into the gateway + blob URLs; port 0 picks a
// free port. dataDir is clank-host's --data-dir, kept separate from the
// shared daemon's so the two host.db files never collide.
func startPreviewGateway(advertiseIP net.IP, port int, dataDir string, lg *log.Logger) (*previewGateway, error) {
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

	userID := previewUserID()
	prov := localprov.New(localprov.Options{
		DataDir: dataDir,
		UserID:  userID,
	}, lg)

	gw, err := gateway.NewGateway(gateway.Config{
		Provisioner: prov,
		Images:      imagesSrv,
	}, lg)
	if err != nil {
		prov.Stop()
		_ = blob.Close()
		return nil, fmt.Errorf("build gateway: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", auth.Middleware(gw.Handler(), &auth.StaticBearer{Token: token, UserID: userID}))

	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		prov.Stop()
		_ = blob.Close()
		return nil, fmt.Errorf("listen on port %d: %w", port, err)
	}

	g := &previewGateway{
		BaseURL: fmt.Sprintf("http://%s:%d", advertiseIP, ln.Addr().(*net.TCPAddr).Port),
		Token:   token,
		UserID:  userID,
		prov:    prov,
		blob:    blob,
		srv:     &http.Server{Handler: mux},
		ln:      ln,
		log:     lg,
	}
	go func() {
		if err := g.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			lg.Printf("preview gateway serve: %v", err)
		}
	}()
	return g, nil
}

// Shutdown stops the HTTP server, the clank-host subprocess (SIGINT→5s→
// SIGKILL via the provisioner), and the blob store.
func (g *previewGateway) Shutdown(ctx context.Context) {
	if err := g.srv.Shutdown(ctx); err != nil {
		g.log.Printf("preview gateway shutdown: %v", err)
	}
	g.prov.Stop()
	_ = g.blob.Close()
}

// previewUserID is the identity the pairing bearer resolves to. Using the
// OS username keeps session attribution sensible and matches the local
// provisioner's UserID so any owner checks line up.
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
