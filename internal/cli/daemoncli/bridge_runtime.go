package daemoncli

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/acksell/clank/internal/bridge"
	"github.com/acksell/clank/internal/config"
	"github.com/acksell/clank/pkg/auth"
	"github.com/acksell/clank/pkg/blobstore"
	"github.com/acksell/clank/pkg/images"
)

// envBridgePort overrides the bridge listener port (default
// bridge.DefaultPort). The port must stay stable across restarts —
// the phone's stored gateway URLs embed it.
const envBridgePort = "CLANK_BRIDGE_PORT"

// bridgeStateFile is the bridge's credential/state file inside
// CLANK_DIR, next to hub.sock and the daemon's other secrets.
const bridgeStateFile = "bridge.json"

// bridgeRuntime owns the daemon's laptop↔phone surface: the secret
// store, the per-address listeners, and the LAN blobstore behind
// /v1/images. Laptop mode only — setupBridge returns nil for cloud
// (TCP) daemons and the rest of the code treats nil as "no bridge".
type bridgeRuntime struct {
	store     *bridge.Store
	listeners *bridge.Listeners
	storage   *swappableStorage
	imagesSrv *images.Server
	port      int
	log       *log.Logger

	blobMu   sync.Mutex
	blob     *blobstore.LAN
	blobHost string
}

// setupBridge opens the bridge store and images plumbing for a laptop
// daemon. TCP (cloud/self-hosted) mode gets nil — no listeners, no
// bridge.json, no routes — pinned by TestSetupBridgeCloudModeIsNil.
// Failures are logged and yield nil: the daemon runs bridgeless
// rather than dying.
func setupBridge(opts ServerOptions, lg *log.Logger) *bridgeRuntime {
	if opts.Listen != "" {
		return nil
	}
	if lg == nil {
		lg = log.Default()
	}
	dir, err := config.Dir()
	if err != nil {
		lg.Printf("bridge: disabled (config dir: %v)", err)
		return nil
	}
	store, err := bridge.OpenStore(filepath.Join(dir, bridgeStateFile))
	if err != nil {
		lg.Printf("bridge: disabled (%v)", err)
		return nil
	}
	storage := &swappableStorage{}
	imagesSrv, err := images.NewServer(images.Config{Storage: storage})
	if err != nil {
		lg.Printf("bridge: disabled (images: %v)", err)
		return nil
	}
	port := bridge.DefaultPort
	if raw := os.Getenv(envBridgePort); raw != "" {
		p, err := strconv.Atoi(raw)
		if err != nil || p < 1 || p > 65535 {
			lg.Printf("bridge: %s=%q invalid, using %d", envBridgePort, raw, port)
		} else {
			port = p
		}
	}
	return &bridgeRuntime{store: store, storage: storage, imagesSrv: imagesSrv, port: port, log: lg}
}

// Images is the server to hang on gateway.Config.Images — built
// before the gateway exists, backed by storage that swaps in once an
// advertisable address is known.
func (b *bridgeRuntime) Images() *images.Server {
	if b == nil {
		return nil
	}
	return b.imagesSrv
}

// Start binds the phone-facing listeners around the in-process
// gateway handler: pre-auth /bridge/ping (the identity probe), then
// the derived-bearer middleware in front of everything else.
func (b *bridgeRuntime) Start(gwHandler http.Handler) {
	if b == nil {
		return
	}
	mux := http.NewServeMux()
	mux.Handle("GET /bridge/ping", bridge.ProbeHandler(b.store))
	mux.Handle("/", auth.Middleware(gwHandler, bridge.NewAuthenticator(b.store, staticUserID(), b.log)))
	b.listeners = bridge.NewListeners(bridge.ListenerOptions{
		Port:    b.port,
		Handler: mux,
		Store:   b.store,
		Log:     b.log,
	})
	b.Refresh(context.Background())
}

// Refresh re-runs discovery, reconciles listeners, and points the
// blobstore at the current phone-reachable address.
func (b *bridgeRuntime) Refresh(ctx context.Context) bridge.Status {
	status := b.listeners.Refresh(ctx)
	b.reconcileBlob(status)
	return status
}

// reconcileBlob keeps the images blobstore advertising an address the
// phone can actually reach: the tailnet bind when present, else a
// trusted-LAN bind, else no storage (uploads unavailable). Rebuilt
// only when the address changes; in-flight blob URLs die on a network
// move (TTL-bounded, accepted v1 wart).
func (b *bridgeRuntime) reconcileBlob(status bridge.Status) {
	host := ""
	for _, bind := range status.Binds {
		if bind.Err != "" || bind.IP == "127.0.0.1" {
			continue
		}
		host = bind.IP
		break // binds are ordered loopback, tailnet, lan — first wins
	}

	b.blobMu.Lock()
	defer b.blobMu.Unlock()
	if host == b.blobHost {
		return
	}
	if b.blob != nil {
		_ = b.blob.Close()
		b.blob = nil
		b.storage.swap(nil)
	}
	b.blobHost = host
	if host == "" {
		return
	}
	signKey := make([]byte, 32)
	if _, err := rand.Read(signKey); err != nil {
		b.log.Printf("bridge: blob sign key: %v", err)
		return
	}
	blob, err := blobstore.NewLAN("0.0.0.0:0", host, signKey)
	if err != nil {
		b.log.Printf("bridge: blob store: %v", err)
		return
	}
	b.blob = blob
	b.storage.swap(blob)
	b.log.Printf("bridge: image uploads via %s", blob.BaseURL())
}

// Close tears down listeners and the blobstore.
func (b *bridgeRuntime) Close() {
	if b == nil {
		return
	}
	if b.listeners != nil {
		b.listeners.Close()
	}
	b.blobMu.Lock()
	defer b.blobMu.Unlock()
	if b.blob != nil {
		_ = b.blob.Close()
		b.blob = nil
	}
}

// bridgeStatusResponse is the admin status payload `clank pair` and
// `clank preview` build QRs from. PairToken carries the root secret —
// this surface is mounted on the unix socket only.
type bridgeStatusResponse struct {
	bridge.Status
	FirstConnected bool     `json:"first_connected"`
	PairToken      string   `json:"pair_token"`
	URLs           []string `json:"urls"`
}

// AdminHandler serves /v1/bridge/* for the local CLI. Nil-safe: a
// bridgeless daemon mounts nothing.
func (b *bridgeRuntime) AdminHandler() http.Handler {
	if b == nil {
		return nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/bridge/status", func(w http.ResponseWriter, r *http.Request) {
		b.writeStatus(w, b.Refresh(r.Context()))
	})
	mux.HandleFunc("POST /v1/bridge/refresh", func(w http.ResponseWriter, r *http.Request) {
		b.writeStatus(w, b.Refresh(r.Context()))
	})
	mux.HandleFunc("POST /v1/bridge/rotate", func(w http.ResponseWriter, r *http.Request) {
		if err := b.store.Rotate(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		b.writeStatus(w, b.Refresh(r.Context()))
	})
	mux.HandleFunc("POST /v1/bridge/trust-network", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Fingerprint string `json:"fingerprint"`
			Label       string `json:"label"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if err := b.store.TrustNetwork(body.Fingerprint, body.Label); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		b.writeStatus(w, b.Refresh(r.Context()))
	})
	return mux
}

func (b *bridgeRuntime) writeStatus(w http.ResponseWriter, status bridge.Status) {
	resp := bridgeStatusResponse{
		Status:         status,
		FirstConnected: b.store.FirstConnected(),
		PairToken:      bridge.EncodeRoot(b.store.Root()),
		URLs:           phoneURLs(status),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// phoneURLs orders the phone-reachable base URLs best-first: MagicDNS
// (survives IP churn), tailnet IP, trusted-LAN IP. Loopback and
// failed binds are excluded. Empty when nothing is reachable yet —
// the CLI's cue to run the trust-this-LAN prompt.
func phoneURLs(status bridge.Status) []string {
	var urls []string
	addURL := func(host string) {
		urls = append(urls, fmt.Sprintf("http://%s", netJoin(host, status.Port)))
	}
	if status.Tailnet != nil {
		for _, bind := range status.Binds {
			if bind.IP == status.Tailnet.IP && bind.Err == "" {
				if status.Tailnet.DNSName != "" {
					addURL(status.Tailnet.DNSName)
				}
				addURL(status.Tailnet.IP)
			}
		}
	}
	for _, bind := range status.Binds {
		if bind.Err == "" && bind.IP == status.LANIP && bind.IP != "127.0.0.1" {
			if status.Tailnet == nil || bind.IP != status.Tailnet.IP {
				addURL(bind.IP)
			}
		}
	}
	return urls
}

func netJoin(host string, port int) string {
	return fmt.Sprintf("%s:%d", host, port)
}

// swappableStorage lets the images server outlive address changes:
// the gateway captures the images.Server at construction, while the
// LAN blobstore underneath is rebuilt whenever the advertisable
// address changes. Nil inner = uploads unavailable (no reachable
// address yet).
type swappableStorage struct {
	mu    sync.RWMutex
	inner blobstore.Storage
}

var errNoBlobStorage = fmt.Errorf("bridge: no reachable address for image uploads yet")

func (s *swappableStorage) swap(inner blobstore.Storage) {
	s.mu.Lock()
	s.inner = inner
	s.mu.Unlock()
}

func (s *swappableStorage) get() (blobstore.Storage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.inner == nil {
		return nil, errNoBlobStorage
	}
	return s.inner, nil
}

func (s *swappableStorage) PresignPut(ctx context.Context, key string, ttl time.Duration) (string, error) {
	inner, err := s.get()
	if err != nil {
		return "", err
	}
	return inner.PresignPut(ctx, key, ttl)
}

func (s *swappableStorage) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	inner, err := s.get()
	if err != nil {
		return "", err
	}
	return inner.PresignGet(ctx, key, ttl)
}

func (s *swappableStorage) Exists(ctx context.Context, key string) (bool, error) {
	inner, err := s.get()
	if err != nil {
		return false, err
	}
	return inner.Exists(ctx, key)
}

func (s *swappableStorage) DeletePrefix(ctx context.Context, prefix string) error {
	inner, err := s.get()
	if err != nil {
		return err
	}
	return inner.DeletePrefix(ctx, prefix)
}
