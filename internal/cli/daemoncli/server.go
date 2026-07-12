package daemoncli

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	daemonclient "github.com/acksell/clank/internal/daemonclient"
	"github.com/acksell/clank/internal/socketutil"
	"github.com/acksell/clank/internal/store"
	"github.com/acksell/clank/pkg/auth"
	"github.com/acksell/clank/pkg/gateway"
	"github.com/acksell/clank/pkg/images"
	"github.com/acksell/clank/pkg/notify"
	"github.com/acksell/clank/pkg/preview/routestore/memstore"
	"github.com/acksell/clank/pkg/provisioner"
)

// openHubListener creates the listener for the configured mode and a
// cleanup func that removes on-disk artifacts.
func openHubListener(opts ServerOptions) (net.Listener, func(), error) {
	if opts.Listen == "" {
		return openUnixListener()
	}
	addr, err := parseTCPListen(opts.Listen)
	if err != nil {
		return nil, nil, err
	}
	return openTCPListener(addr)
}

func openUnixListener() (net.Listener, func(), error) {
	sockPath, err := daemonclient.SocketPath()
	if err != nil {
		return nil, nil, fmt.Errorf("socket path: %w", err)
	}
	// Probe before unlink so we don't yank an active peer's listener.
	if conn, dialErr := net.DialTimeout("unix", sockPath, 200*time.Millisecond); dialErr == nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("clankd already running on %s", sockPath)
	}
	if err := socketutil.RemoveStale(sockPath); err != nil {
		return nil, nil, err
	}
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, nil, fmt.Errorf("listen %s: %w", sockPath, err)
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		listener.Close()
		_ = socketutil.RemoveStale(sockPath)
		return nil, nil, fmt.Errorf("chmod socket: %w", err)
	}
	cleanup := func() {
		if err := socketutil.RemoveStale(sockPath); err != nil {
			log.Printf("socket cleanup: %v", err)
		}
	}
	return listener, cleanup, nil
}

func openTCPListener(addr string) (net.Listener, func(), error) {
	if conn, dialErr := net.DialTimeout("tcp", addr, 200*time.Millisecond); dialErr == nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("address already in use: %s", addr)
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("listen tcp %s: %w", addr, err)
	}
	return listener, func() {}, nil
}

// parseTCPListen accepts "tcp://host:port" or "host:port" and returns the
// host:port suitable for net.Listen("tcp", ...).
func parseTCPListen(s string) (string, error) {
	if strings.HasPrefix(s, "tcp://") {
		s = strings.TrimPrefix(s, "tcp://")
	}
	if _, _, err := net.SplitHostPort(s); err != nil {
		return "", fmt.Errorf("invalid --listen %q (want tcp://host:port): %w", s, err)
	}
	return s, nil
}

// runGatewayServer mounts the daemon gateway on opts.Listen.
//
// Modes:
//   - Unix socket (default): laptop mode. File mode is the gate.
//   - TCP (opts.Listen non-empty): self-hosted/cloud mode. Auth selected
//     by opts.Auth when set, else by env (resolveDefaultAuth).
//
// Both modes write the PID file at daemonclient.PIDPath().
func runGatewayServer(prov provisioner.Provisioner, st *store.Store, opts ServerOptions) error {
	pidPath, err := daemonclient.PIDPath()
	if err != nil {
		return fmt.Errorf("pid path: %w", err)
	}

	var imagesSrv *images.Server
	if opts.Listen != "" {
		// Image uploads use their own bucket (CLANK_IMAGES_S3_*).
		// nil when unset — POST /v1/images simply 404s.
		imagesSrv, err = loadImagesFromEnv(context.Background())
		if err != nil {
			return fmt.Errorf("build images server: %w", err)
		}
		if imagesSrv != nil {
			log.Printf("gateway: image uploads enabled (S3 bucket=%s)", os.Getenv("CLANK_IMAGES_S3_BUCKET"))
		}
	}

	// Build the notification dispatcher. The gateway mounts the
	// user-scoped /devices routes inside its Handler() (they inherit
	// the outer auth wrap); the host-→-clankd webhook is exposed
	// pre-auth via gw.NotifyWebhookHandler() below.
	dispatcher := notify.NewDispatcher(st, notifyDeviceAdapter{s: st}, notify.New(log.Default()), log.Default())

	gwCfg := gateway.Config{
		Provisioner: prov,
		Images:      imagesSrv,
		AuthConfig:  loadAuthConfigFromEnv(),
		Notify:      dispatcher,
	}

	// Wire the preview surface when CLANK_PREVIEW_ROOT_DOMAIN is set.
	// For laptop/docker dev we use an in-process memstore for routes
	// (the cloud path uses Postgres via supaclank's pgx adapter). The
	// local provisioner also satisfies gateway.PreviewHostLookup —
	// when clank-host calls the register webhook with its per-host
	// notifier_token bearer, the provisioner resolves it to a
	// synthetic Host{}. Both halves are nil-safe: empty domain
	// disables the entire surface and the gateway falls back to
	// today's path-routed behavior.
	if rootDomain := os.Getenv("CLANK_PREVIEW_ROOT_DOMAIN"); rootDomain != "" {
		var lookup gateway.PreviewHostLookup
		if local, ok := prov.(gateway.PreviewHostLookup); ok {
			lookup = local
		}
		if lookup == nil {
			log.Printf("gateway: CLANK_PREVIEW_ROOT_DOMAIN set but the active provisioner doesn't implement GetHostByNotifierToken — preview surface disabled")
		} else {
			gwCfg.PreviewRoutes = memstore.New(nil)
			gwCfg.PreviewHostLookup = lookup
			gwCfg.PreviewRootDomain = rootDomain
			// Owner-only token authentication reuses whatever
			// Authenticator the rest of the daemon uses. Resolved
			// below (`authenticator`) — but we need it now, so
			// re-resolve in-place. Cheap (no I/O on the JWT path).
			authForPreview := opts.Auth
			if authForPreview == nil {
				ctxAuth := context.Background()
				resolved, _, err := resolveDefaultAuth(ctxAuth, opts)
				if err != nil {
					return fmt.Errorf("preview authenticator: %w", err)
				}
				authForPreview = resolved
			}
			gwCfg.PreviewAuthenticator = authForPreview
			if hexKey := os.Getenv("CLANK_PREVIEW_SIGNING_KEY"); hexKey != "" {
				key, err := hex.DecodeString(hexKey)
				if err != nil {
					return fmt.Errorf("CLANK_PREVIEW_SIGNING_KEY: %w", err)
				}
				gwCfg.PreviewSigningKey = key
			}
			log.Printf("gateway: preview surface enabled on *.%s", rootDomain)
		}
	}
	gw, err := gateway.NewGateway(gwCfg, log.Default())
	if err != nil {
		return fmt.Errorf("build gateway: %w", err)
	}

	authenticator := opts.Auth
	authDesc := "auth.Authenticator (embedder-supplied)"
	if authenticator == nil {
		ctx := context.Background()
		authenticator, authDesc, err = resolveDefaultAuth(ctx, opts)
		if err != nil {
			return err
		}
	}
	logAuthMode(authDesc)

	// Wire pre-auth routes (no user bearer required) on a parent mux,
	// then mount the auth-wrapped gateway as the catch-all.
	mux := http.NewServeMux()
	if h := gw.NotifyWebhookHandler(); h != nil {
		mux.Handle("POST /webhooks/notifications", h)
	}
	if h := gw.PreviewWebhookHandler(); h != nil {
		mux.Handle("/webhooks/preview/", h)
	}
	if ach := gw.AuthConfigHandler(); ach != nil {
		mux.Handle("GET /auth-config", ach)
		log.Printf("gateway: /auth-config discovery enabled")
	}
	mux.Handle("/", auth.Middleware(gw.Handler(), authenticator))
	// Wrap with the preview-subdomain dispatcher so requests to
	// preview-<token>.<root> reach the tokenized proxy before the
	// auth middleware fires. No-op when CLANK_PREVIEW_ROOT_DOMAIN
	// is unset.
	var handler http.Handler = gw.WrapPreviewSubdomain(mux)

	listener, cleanup, err := openHubListener(opts)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		listener.Close()
		return fmt.Errorf("write PID file: %w", err)
	}
	defer os.Remove(pidPath)

	srv := &http.Server{Handler: handler}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	serveErr := make(chan error, 1)
	go func() {
		log.Printf("gateway listening on %s", listener.Addr())
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			serveErr <- err
		}
		close(serveErr)
	}()

	select {
	case sig := <-sigCh:
		log.Printf("received signal %v, shutting down gateway", sig)
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("gateway serve: %w", err)
		}
	}

	shutdownCtx, sc := context.WithTimeout(context.Background(), 5*time.Second)
	defer sc()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("gateway shutdown: %v", err)
	}
	return nil
}


// loadAuthConfigFromEnv builds a gateway.AuthConfig from CLANK_AUTH_*
// env vars for the dev / docker-stack path. Returns nil when the core
// endpoints aren't set — embedders (self-hoster, etc.) populate
// gateway.Config.AuthConfig directly and don't need these env vars.
//
// CLANK_AUTH_AUTHORIZE_ENDPOINT — full URL, e.g. http://stub/authorize
// CLANK_AUTH_TOKEN_ENDPOINT     — full URL, e.g. http://stub/token
// CLANK_AUTH_CLIENT_ID          — OAuth client identifier
// CLANK_AUTH_SCOPES             — space-separated, e.g. "openid email"
// CLANK_AUTH_DEFAULT_PROVIDER   — optional IdP hint (e.g. "github")
// CLANK_AUTH_CALLBACK_PORT      — pin laptop's PKCE listener to a
//
//	fixed port (required when the IdP
//	matches redirect_uris strictly,
//	e.g. Supabase OAuth Server).
func loadAuthConfigFromEnv() *gateway.AuthConfig {
	authorize := os.Getenv("CLANK_AUTH_AUTHORIZE_ENDPOINT")
	token := os.Getenv("CLANK_AUTH_TOKEN_ENDPOINT")
	clientID := os.Getenv("CLANK_AUTH_CLIENT_ID")
	if authorize == "" || token == "" || clientID == "" {
		return nil
	}
	cfg := &gateway.AuthConfig{
		AuthorizeEndpoint: authorize,
		TokenEndpoint:     token,
		ClientID:          clientID,
		DefaultProvider:   os.Getenv("CLANK_AUTH_DEFAULT_PROVIDER"),
	}
	if s := os.Getenv("CLANK_AUTH_SCOPES"); s != "" {
		cfg.Scopes = strings.Fields(s)
	}
	if s := os.Getenv("CLANK_AUTH_CALLBACK_PORT"); s != "" {
		p, err := strconv.Atoi(s)
		if err != nil || p < 1 || p > 65535 {
			log.Printf("gateway: CLANK_AUTH_CALLBACK_PORT=%q invalid, ignoring", s)
		} else {
			cfg.CallbackPort = p
		}
	}
	return cfg
}
