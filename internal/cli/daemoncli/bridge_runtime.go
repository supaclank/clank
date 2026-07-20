package daemoncli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	auth      *bridge.Authenticator
	pairing   *bridge.Pairing
	storage   *swappableStorage
	imagesSrv *images.Server
	port      int
	log       *log.Logger

	// newBlob constructs the LAN blobstore; blobstore.NewLAN by
	// default, overridable in tests to exercise reconcileBlob's
	// failure/retry path.
	newBlob func(bindAddr, advertiseHost string, signKey []byte) (*blobstore.LAN, error)

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
	return &bridgeRuntime{store: store, storage: storage, imagesSrv: imagesSrv, port: port, log: lg, newBlob: blobstore.NewLAN}
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
// the signature-verifying middleware in front of everything else.
func (b *bridgeRuntime) Start(gwHandler http.Handler) {
	if b == nil {
		return
	}
	b.auth = bridge.NewAuthenticator(b.store, staticUserID(), b.log, nil)
	b.pairing = bridge.NewPairing(b.store, nil)
	mux := http.NewServeMux()
	mux.Handle("GET /bridge/ping", bridge.ProbeHandler(b.store))
	// Pre-auth pairing ceremony: a new phone (unapproved key) commits,
	// reveals, and polls for approval. Window-gated + capped in Pairing.
	mux.Handle("POST /bridge/pair/begin", b.pairBeginHandler())
	mux.Handle("POST /bridge/pair/reveal", b.pairRevealHandler())
	mux.Handle("GET /bridge/pair/attempt", b.pairAttemptHandler())
	// Signed-request-only: mints the static short-TTL bearer the native
	// preview overlay authenticates with (it can't run the signer).
	mux.Handle("POST /bridge/session-token", auth.Middleware(b.sessionTokenHandler(), b.auth))
	mux.Handle("/", auth.Middleware(gwHandler, b.auth))
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
	if host == "" {
		b.blobHost = ""
		return
	}
	signKey := make([]byte, 32)
	if _, err := rand.Read(signKey); err != nil {
		b.log.Printf("bridge: blob sign key: %v", err)
		b.blobHost = ""
		return
	}
	blob, err := b.newBlob("0.0.0.0:0", host, signKey)
	if err != nil {
		b.log.Printf("bridge: blob store: %v", err)
		b.blobHost = ""
		return
	}
	b.blobHost = host
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
// `clank preview` build QRs from — all public: the host public key,
// the approved-device registry, and the reachable URLs.
type bridgeStatusResponse struct {
	bridge.Status
	HostKey         string                `json:"host_key"`
	Devices         []bridge.DeviceRecord `json:"devices"`
	URLs            []string              `json:"urls"`
	LastDevice      string                `json:"last_device,omitempty"`
	LastConnectedAt *time.Time            `json:"last_connected_at,omitempty"`
}

// AdminHandler serves /v1/bridge/* for the local CLI. Nil-safe: a
// bridgeless daemon mounts nothing.
func (b *bridgeRuntime) AdminHandler() http.Handler {
	if b == nil {
		return nil
	}
	mux := http.NewServeMux()
	// TODO(ai-review): throttle re-discovery — `clank pair`'s 1Hz poll
	// currently forces a full tailscale/route/arp exec on every GET.
	// https://github.com/Acksell/clank/pull/175#discussion_r3609118211
	mux.HandleFunc("GET /v1/bridge/status", func(w http.ResponseWriter, r *http.Request) {
		b.writeStatus(w, b.Refresh(r.Context()))
	})
	mux.HandleFunc("POST /v1/bridge/refresh", func(w http.ResponseWriter, r *http.Request) {
		b.writeStatus(w, b.Refresh(r.Context()))
	})
	// Revoke one device by public key, or every device at once. The
	// host key stays either way — returning phones still recognize the
	// laptop, they just have to re-pair.
	mux.HandleFunc("POST /v1/bridge/pair/revoke", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			PubKey string `json:"pubkey"`
			All    bool   `json:"all"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		switch {
		case body.All:
			if _, err := b.store.RemoveAllDevices(); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		case body.PubKey != "":
			pub, err := bridge.DecodeKey(body.PubKey)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			removed, err := b.store.RemoveDevice(pub)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if !removed {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such device"})
				return
			}
		default:
			http.Error(w, "pubkey or all required", http.StatusBadRequest)
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
	// Pairing ceremony admin (unix socket = trusted local): the CLI
	// leases the window by polling, and submits the code the laptop
	// user typed. Approval can only happen here, physically local.
	mux.HandleFunc("POST /v1/bridge/pair/poll", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"pending": b.pairing.RefreshWindow()})
	})
	mux.HandleFunc("POST /v1/bridge/pair/complete", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		device, err := b.pairing.Complete(body.Code)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"device": device})
	})
	return mux
}

// maxPairBeginBody caps the pre-auth /bridge/pair/begin body (device
// name + a fixed-length commit hex) against a DoS from oversized reads.
const maxPairBeginBody = 4096

// pairBeginHandler opens an attempt for a scanning phone (its name +
// commitment), pre-auth on the bridge listener, and returns the daemon
// nonce plus a host-key signature the phone verifies against hk.
// Window/cap/lockout failures map to 409/429 so the phone can message
// precisely.
func (b *bridgeRuntime) pairBeginHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxPairBeginBody)
		var body struct {
			Device string `json:"device"`
			Commit string `json:"commit"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body) // commit validated in Begin
		id, nonceD, replySig, err := b.pairing.Begin(body.Device, body.Commit)
		if err != nil {
			status := http.StatusConflict
			switch {
			case errors.Is(err, bridge.ErrPairTooManyPending), errors.Is(err, bridge.ErrPairLockedOut):
				status = http.StatusTooManyRequests
			case errors.Is(err, bridge.ErrPairBadCommit):
				status = http.StatusBadRequest
			}
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"attempt_id": id,
			"nonce_d":    hex.EncodeToString(nonceD),
			"reply_sig":  replySig,
		})
	})
}

// pairRevealHandler opens the phone's commitment: it verifies the
// revealed device key + nonce hash to the commit, after which both
// sides can derive the SAS. A reveal that doesn't open the commit is a
// 400 (the attempt is burned server-side).
func (b *bridgeRuntime) pairRevealHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			AttemptID string `json:"attempt_id"`
			DevicePub string `json:"device_pub"`
			NonceP    string `json:"nonce_p"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}
		pub, keyErr := bridge.DecodeKey(body.DevicePub)
		nonceP, nonceErr := hex.DecodeString(body.NonceP)
		if keyErr != nil || nonceErr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": bridge.ErrPairBadKey.Error()})
			return
		}
		if err := b.pairing.Reveal(body.AttemptID, pub, nonceP); err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, bridge.ErrPairNoAttempt) {
				status = http.StatusNotFound
			}
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
	})
}

// pairAttemptHandler reports an attempt's state to the polling phone.
// Approval carries no payload — the phone's key is now trusted and its
// next signed request just works.
func (b *bridgeRuntime) pairAttemptHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := b.pairing.PollAttempt(r.URL.Query().Get("id"))
		writeJSON(w, http.StatusOK, map[string]string{"state": string(state)})
	})
}

// sessionTokenHandler mints the native overlay's bearer. Runs behind
// auth.Middleware, but only a SIGNED request qualifies: the signature
// headers must be present (a session token can't mint a session
// token), and the mint is bound to the signing device.
func (b *bridgeRuntime) sessionTokenHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pub, err := bridge.DecodeKey(r.Header.Get(bridge.HeaderKey))
		if err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "session tokens are minted by signed requests only"})
			return
		}
		token, expiresAt, err := b.auth.MintSessionToken(pub)
		if err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "unknown device"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"token":      token,
			"expires_at": expiresAt.UTC().Format(time.RFC3339),
		})
	})
}

// writeJSON is the bridge admin/pairing handlers' small response helper.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (b *bridgeRuntime) writeStatus(w http.ResponseWriter, status bridge.Status) {
	resp := bridgeStatusResponse{
		Status:  status,
		HostKey: bridge.EncodeKey(b.store.HostPublicKey()),
		Devices: b.store.Devices(),
		URLs:    phoneURLs(status),
	}
	if b.auth != nil {
		if device, at := b.auth.LastConnection(); !at.IsZero() {
			resp.LastDevice = device
			resp.LastConnectedAt = &at
		}
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

var errNoBlobStorage = fmt.Errorf("bridge: no reachable address for image uploads yet: %w", blobstore.ErrUnavailable)

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
