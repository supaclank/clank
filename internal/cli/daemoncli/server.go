package daemoncli

import (
	"context"
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
	"github.com/acksell/clank/pkg/auth"
	"github.com/acksell/clank/pkg/gateway"
	"github.com/acksell/clank/pkg/provisioner"
	clanksync "github.com/acksell/clank/pkg/sync"
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
//   - Unix socket (default): laptop mode. File mode is the gate. Sync
//     stays nil — laptop has no S3 access and exposes no sync routes.
//     Push/pull goes through the cloud gateway (prefs.Remote.GatewayURL).
//   - TCP (opts.Listen non-empty): self-hosted/cloud mode. Auth selected
//     by opts.Auth when set, else by env (resolveDefaultAuth). If
//     CLANK_SYNC_S3_BUCKET is set, an embedded sync.Server is built from
//     CLANK_SYNC_S3_* env and mounted under /v1/.
//
// Both modes write the PID file at daemonclient.PIDPath().
func runGatewayServer(prov provisioner.Provisioner, opts ServerOptions) error {
	pidPath, err := daemonclient.PIDPath()
	if err != nil {
		return fmt.Errorf("pid path: %w", err)
	}

	var syncSrv *clanksync.Server
	if opts.Listen != "" {
		// TCP mode: build the embedded sync server when CLANK_SYNC_S3_*
		// env vars are present. Returns nil when unset (TCP without sync
		// still works — useful for proxy-only hubs).
		syncSrv, err = loadSyncFromEnv(context.Background(), log.Default())
		if err != nil {
			return fmt.Errorf("build sync server: %w", err)
		}
		if syncSrv != nil {
			log.Printf("gateway: embedded sync server enabled (S3 bucket=%s)", os.Getenv("CLANK_SYNC_S3_BUCKET"))
		}
	}

	gwCfg := gateway.Config{
		Provisioner: prov,
		Sync:        syncSrv,
		AuthConfig:  loadAuthConfigFromEnv(),
	}
	// Laptop mode (Sync == nil): wire the per-session router so
	// /sessions/* routes between local clank-host and the active
	// remote based on worktree ownership. Cloud mode (Sync != nil)
	// stays pure-proxy — it IS the destination of the laptop's
	// proxy, so it has no "active remote" upstream of itself.
	if syncSrv == nil {
		resolver := newPrefsRemoteResolver(log.Default())
		gwCfg.RemoteResolver = resolver
		gwCfg.OwnerCache = gateway.NewOwnerCache(resolver, nil)
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

	// Wire the auth-config discovery route PRE-auth (clank has no
	// token when it fetches it). Falls back to a single auth-wrapped
	// handler when no AuthConfig is configured.
	var handler http.Handler = auth.Middleware(gw.Handler(), authenticator)
	if ach := gw.AuthConfigHandler(); ach != nil {
		mux := http.NewServeMux()
		mux.Handle("GET /auth-config", ach)
		mux.Handle("/", handler)
		handler = mux
		log.Printf("gateway: /auth-config discovery enabled")
	}

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
//                                  fixed port (required when the IdP
//                                  matches redirect_uris strictly,
//                                  e.g. Supabase OAuth Server).
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
