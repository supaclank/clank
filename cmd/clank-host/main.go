// clank-host is the Host plane binary. It owns the BackendManagers and
// SessionBackends and serves the host HTTP API on either a Unix socket
// (default for laptop hubs) or a TCP listener (for cloud sandboxes /
// in-process LocalLauncher tests).
//
// In production it is spawned as a child process by clankd (the Hub).
// clankd connects via internal/host/client and routes every HUB-tagged
// operation through the wire.
//
// Usage:
//
//	clank-host --socket /path/to/host.sock
//	clank-host --listen tcp://127.0.0.1:0    # auto-pick port
//
// On startup prints "listening on <addr>" — parents that picked port 0
// read this to discover the bound address.
//
// On SIGINT/SIGTERM the server shuts down gracefully and host.Service
// stops every registered backend.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
	hostmux "github.com/acksell/clank/internal/host/mux"
	"github.com/acksell/clank/internal/host/preview"
	hoststore "github.com/acksell/clank/internal/host/store"
	"github.com/acksell/clank/internal/keepalive"
	keepaliveexit "github.com/acksell/clank/internal/keepalive/exit"
	keepalivenoop "github.com/acksell/clank/internal/keepalive/noop"
	"github.com/acksell/clank/internal/keepalive/sprites"
	"github.com/acksell/clank/internal/notifier"
	notifiernoop "github.com/acksell/clank/internal/notifier/noop"
	"github.com/acksell/clank/internal/notifier/webhook"
	"github.com/acksell/clank/internal/socketutil"
)

const (
	keepaliveProviderNone    = "none"
	keepaliveProviderNoop    = "noop"
	keepaliveProviderSprites = "sprites"
	keepaliveProviderExit    = "exit"

	notifierProviderNone    = "none"
	notifierProviderNoop    = "noop"
	notifierProviderWebhook = "webhook"
)

func main() {
	// Subcommand dispatch precedes flag parsing — `clank-host
	// git-credential get` is invoked BY GIT (per repo config, see
	// internal/host/github/git_credential.go) and takes none of the
	// serve flags.
	if len(os.Args) > 1 && os.Args[1] == gitCredentialCommand {
		os.Exit(runGitCredential(os.Args[2:]))
	}
	// `clank-host print-pins` — self-describe the pinned toolchain so an
	// image build (clank's or an operator's) installs exactly these.
	if len(os.Args) > 1 && os.Args[1] == printPinsCommand {
		os.Exit(runPrintPins())
	}

	socket := flag.String("socket", "", "Path to Unix socket to listen on (mutually exclusive with --listen)")
	listen := flag.String("listen", "", "Listener address: tcp://host:port (use :0 for auto-pick) or unix:///path. Mutually exclusive with --socket.")
	listenAuthToken := flag.String("listen-auth-token", os.Getenv("CLANK_HOST_AUTH_TOKEN"), "Bearer token required on every HTTP request. Empty disables the check (laptop-local mode). Defaults to $CLANK_HOST_AUTH_TOKEN.")
	dataDir := flag.String("data-dir", os.Getenv("CLANK_HOST_DATA_DIR"), "Directory for host-side persistent state (host.db). Defaults to $CLANK_HOST_DATA_DIR; if neither is set, falls back to $HOME/.clank-host. PR 3+ stores session metadata here.")
	keepaliveProvider := flag.String("keepalive-provider", envDefault("CLANK_KEEPALIVE_PROVIDER", keepaliveProviderNone), "Provider that receives keepalive Ticks while sessions emit events: 'sprites' (Fly Sprites Tasks API), 'exit' (shut down when idle — machine-style sandboxes), 'noop' (debug), or 'none' (disabled — laptop default). Defaults to $CLANK_KEEPALIVE_PROVIDER.")
	notifierProvider := flag.String("notifier-provider", envDefault("CLANK_NOTIFIER_PROVIDER", notifierProviderNone), "Provider that receives notification-worthy events (idle, permission, error): 'webhook' (POST to --notifier-webhook-url), 'noop' (debug), or 'none' (disabled — laptop default). Defaults to $CLANK_NOTIFIER_PROVIDER.")
	notifierWebhookURL := flag.String("notifier-webhook-url", os.Getenv("CLANK_NOTIFIER_WEBHOOK_URL"), "POST target when --notifier-provider=webhook. Defaults to $CLANK_NOTIFIER_WEBHOOK_URL.")
	notifierWebhookToken := flag.String("notifier-webhook-token", os.Getenv("CLANK_NOTIFIER_TOKEN"), "Per-host bearer token sent as 'Authorization: Bearer <token>' to the webhook target. Defaults to $CLANK_NOTIFIER_TOKEN.")
	previewWebhookURL := flag.String("preview-webhook-url", os.Getenv("CLANK_PREVIEW_WEBHOOK_URL"), "Gateway base for the preview register/revoke webhooks (e.g. https://api.example.dev/webhooks/preview). Empty disables gateway integration — preview servers still spawn but no public token is minted (laptop dev). Defaults to $CLANK_PREVIEW_WEBHOOK_URL.")
	githubOAuthClientID := flag.String("github-oauth-client-id", os.Getenv("CLANK_GITHUB_OAUTH_CLIENT_ID"), "Clank GitHub OAuth App client_id, used for the GitHub Connect device flow. Empty disables GitHub Connect on this host. Defaults to $CLANK_GITHUB_OAUTH_CLIENT_ID.")
	projectCommitterName := flag.String("project-committer-name", os.Getenv("CLANK_PROJECT_COMMITTER_NAME"), "Git committer name stamped on a scaffolded project's seed commit. Empty uses a neutral default. Defaults to $CLANK_PROJECT_COMMITTER_NAME.")
	projectCommitterEmail := flag.String("project-committer-email", os.Getenv("CLANK_PROJECT_COMMITTER_EMAIL"), "Git committer email stamped on a scaffolded project's seed commit. Empty uses a neutral default. Defaults to $CLANK_PROJECT_COMMITTER_EMAIL.")
	templatesJSON := flag.String("templates-json", os.Getenv("CLANK_TEMPLATES"), "JSON array of builtin create-project templates ([{\"display_name\":...,\"clone_url\":...}]). Served by GET /templates alongside the user's GitHub template repos. Empty disables builtin templates. Defaults to $CLANK_TEMPLATES.")
	localFileAttachments := flag.Bool("local-file-attachments", false, "Honor file:// image attachment sources (the client shares this host's filesystem). Set by the local laptop provisioner; off for remote sprites so a message can't make the host read arbitrary local paths.")
	ghCLIAuth := flag.Bool("gh-cli-auth", false, "Resolve GitHub tokens from the machine's gh CLI login (gh auth token) when no clank GitHub connection exists. Set by the local laptop provisioner; off for remote sprites, which have no gh login to borrow.")
	flag.Parse()

	if *socket == "" && *listen == "" {
		fmt.Fprintln(os.Stderr, "clank-host: --socket or --listen is required")
		os.Exit(2)
	}
	if *socket != "" && *listen != "" {
		fmt.Fprintln(os.Stderr, "clank-host: --socket and --listen are mutually exclusive")
		os.Exit(2)
	}

	// file:// image attachments are honored only when the client shares this
	// host's filesystem (the laptop), as signaled by the local provisioner. A
	// remote sprite leaves this off so a message can't make it read arbitrary
	// local paths.
	agent.AllowLocalFileAttachments = *localFileAttachments

	// Refuse to start with a non-loopback TCP listener and no auth
	// token — that combo would expose every clank-host endpoint to
	// the network. LocalLauncher uses 127.0.0.1; in-sprite uses an
	// explicit token, so production paths are unaffected.
	if *listen != "" && *listenAuthToken == "" {
		if err := requireLoopbackTCP(*listen); err != nil {
			fmt.Fprintln(os.Stderr, "clank-host:", err)
			os.Exit(2)
		}
	}

	addr := *listen
	if addr == "" {
		addr = "unix://" + *socket
	}
	cfg := runConfig{
		addr:                  addr,
		templatesJSON:         *templatesJSON,
		listenAuthToken:       *listenAuthToken,
		dataDir:               *dataDir,
		keepaliveProvider:     *keepaliveProvider,
		notifierProvider:      *notifierProvider,
		notifierWebhookURL:    *notifierWebhookURL,
		notifierWebhookToken:  *notifierWebhookToken,
		previewWebhookURL:     *previewWebhookURL,
		githubOAuthClientID:   *githubOAuthClientID,
		ghCLIAuth:             *ghCLIAuth,
		projectCommitterName:  *projectCommitterName,
		projectCommitterEmail: *projectCommitterEmail,
	}
	if err := run(cfg); err != nil {
		log.Fatalf("clank-host: %v", err)
	}
}

// runConfig bundles the flag-derived settings passed into run. Each
// new subsystem (keepalive, notifier, …) adds another knob, and a
// positional-argument function would have grown into an
// argument-order trap; this is the seam for "future-me adds a flag
// without touching every test".
type runConfig struct {
	addr                  string
	templatesJSON         string
	listenAuthToken       string
	dataDir               string
	keepaliveProvider     string
	notifierProvider      string
	notifierWebhookURL    string
	notifierWebhookToken  string
	previewWebhookURL     string
	githubOAuthClientID   string
	ghCLIAuth             bool
	projectCommitterName  string
	projectCommitterEmail string
}

// buildKeepaliveListener constructs the provider-specific Listener from
// the --keepalive-provider value. Returns nil for "none" so the host
// service skips wiring entirely. shutdown initiates a graceful process
// shutdown; only the "exit" provider uses it.
func buildKeepaliveListener(provider string, shutdown func(), lg *log.Logger) (keepalive.Listener, error) {
	switch provider {
	case keepaliveProviderNone:
		return nil, nil
	case keepaliveProviderNoop:
		return keepalivenoop.Listener{}, nil
	case keepaliveProviderSprites:
		return sprites.New(lg), nil
	case keepaliveProviderExit:
		if shutdown == nil {
			return nil, fmt.Errorf("--keepalive-provider=%s requires a shutdown function", keepaliveProviderExit)
		}
		return keepaliveexit.New(keepaliveexit.Options{Shutdown: shutdown, Log: lg}), nil
	default:
		return nil, fmt.Errorf("unknown --keepalive-provider %q (want %s|%s|%s|%s)", provider, keepaliveProviderSprites, keepaliveProviderExit, keepaliveProviderNoop, keepaliveProviderNone)
	}
}

// envDefault returns the env var's value when it's non-empty, else fallback.
func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// buildNotifierProvider constructs the provider-specific notifier
// Provider from the --notifier-provider value. Returns nil for "none"
// so the host service skips wiring entirely.
func buildNotifierProvider(provider, webhookURL, webhookToken string, lg *log.Logger) (notifier.Provider, error) {
	switch provider {
	case notifierProviderNone:
		return nil, nil
	case notifierProviderNoop:
		return notifiernoop.New(lg), nil
	case notifierProviderWebhook:
		if webhookURL == "" {
			return nil, fmt.Errorf("--notifier-provider=webhook requires --notifier-webhook-url")
		}
		if webhookToken == "" {
			return nil, fmt.Errorf("--notifier-provider=webhook requires --notifier-webhook-token (or $CLANK_NOTIFIER_TOKEN)")
		}
		return webhook.New(webhookURL, webhookToken, lg), nil
	default:
		return nil, fmt.Errorf("unknown --notifier-provider %q (want %s|%s|%s)", provider, notifierProviderWebhook, notifierProviderNoop, notifierProviderNone)
	}
}

// requireLoopbackTCP returns an error when the parsed --listen address
// is a non-loopback TCP bind. unix://, host==loopback, and host==""
// (which net.Listen treats as 0.0.0.0 — disallowed here without a
// token) are the cases callers care about.
func requireLoopbackTCP(listen string) error {
	if !strings.HasPrefix(listen, "tcp://") {
		// Non-tcp schemes (unix://) are gated by file mode, not auth.
		return nil
	}
	hostPort := strings.TrimPrefix(listen, "tcp://")
	host, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		return fmt.Errorf("invalid --listen %q: %w", listen, err)
	}
	if isLoopbackHost(host) {
		return nil
	}
	return fmt.Errorf("--listen tcp://%s requires --listen-auth-token (or $CLANK_HOST_AUTH_TOKEN); refusing to expose unauthenticated host on a non-loopback bind", hostPort)
}

// isLoopbackHost reports whether host resolves to a loopback address.
// Empty host means "all interfaces" (0.0.0.0/::), which we treat as
// non-loopback to refuse anonymous wide-open binds.
func isLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	// Hostname that isn't "localhost" — resolve with a short timeout
	// so a flaky resolver can't hang startup. Treat any failure (or
	// any non-loopback resolution) as non-loopback.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(addrs) == 0 {
		return false
	}
	for _, a := range addrs {
		if !a.IP.IsLoopback() {
			return false
		}
	}
	return true
}

// resolveDataDir returns the host's persistent data directory.
// Resolution: explicit flag > $CLANK_HOST_DATA_DIR (handled by flag
// default) > $HOME/.clank-host. Creates the directory if missing.
func resolveDataDir(dataDir string) (string, error) {
	if dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		dataDir = filepath.Join(home, ".clank-host")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", fmt.Errorf("create data dir %s: %w", dataDir, err)
	}
	return dataDir, nil
}

// run binds the listener for cfg.addr (a "tcp://host:port" or
// "unix:///path" URL) and serves the host API on it until
// SIGINT/SIGTERM.
func run(cfg runConfig) error {
	lg := log.New(os.Stderr, "[clank-host] ", log.LstdFlags)

	// The exit provider stops the process when idle; routing through a
	// self-SIGTERM reuses the exact signal path an operator kill takes
	// (graceful HTTP drain + backend shutdown below).
	selfTerminate := func() {
		p, err := os.FindProcess(os.Getpid())
		if err != nil {
			lg.Printf("keepalive: self-terminate: %v", err)
			return
		}
		if err := p.Signal(syscall.SIGTERM); err != nil {
			lg.Printf("keepalive: self-terminate signal: %v", err)
		}
	}
	keepaliveListener, err := buildKeepaliveListener(cfg.keepaliveProvider, selfTerminate, lg)
	if err != nil {
		return err
	}
	notifierProvider, err := buildNotifierProvider(cfg.notifierProvider, cfg.notifierWebhookURL, cfg.notifierWebhookToken, lg)
	if err != nil {
		return err
	}
	var notifierLoop *notifier.Loop
	if notifierProvider != nil {
		notifierLoop = notifier.New(notifier.Config{Provider: notifierProvider, Log: lg})
	}

	ln, kind, sockPath, err := openListener(cfg.addr)
	if err != nil {
		return err
	}
	// Free the listener on any early-return path before we hand it to
	// srv.Serve. Once Serve owns it, srv.Shutdown closes via the same
	// listener and this Close becomes a no-op.
	defer func() { _ = ln.Close() }()

	// PR 3: open the host's persistent SQLite for session metadata.
	// Crash on init failure — running without persistence would
	// silently lose session state. The host store lives separately
	// from the daemon's clank.db (which is the provisioner's host
	// registry).
	resolvedDataDir, err := resolveDataDir(cfg.dataDir)
	if err != nil {
		return fmt.Errorf("resolve data dir: %w", err)
	}
	dbPath := filepath.Join(resolvedDataDir, "host.db")
	hostStore, err := hoststore.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open host store %s: %w", dbPath, err)
	}
	defer hostStore.Close()
	lg.Printf("host store opened at %s", dbPath)

	// Build the gateway webhook client. Empty preview-webhook-url
	// disables the integration; the gwclient methods become no-ops
	// and Manager keeps spawning local Metro without minting a
	// public URL — useful for laptop dev or any sprite that hasn't
	// been told about the gateway yet.
	gwClient := preview.NewGWClient(cfg.previewWebhookURL, cfg.notifierWebhookToken)

	templates, err := parseTemplatesJSON(cfg.templatesJSON)
	if err != nil {
		return fmt.Errorf("parse --templates-json: %w", err)
	}

	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode:   host.NewOpenCodeBackendManager(),
			agent.BackendClaudeCode: host.NewClaudeBackendManager(),
		},
		Log:                   lg,
		Templates:             templates,
		SessionsStore:         hostStore,
		KeepaliveListener:     keepaliveListener,
		NotifierLoop:          notifierLoop,
		PreviewGWClient:       gwClient,
		GitHubOAuthClientID:   cfg.githubOAuthClientID,
		GitHubGhCLIAuth:       cfg.ghCLIAuth,
		ProjectCommitterName:  cfg.projectCommitterName,
		ProjectCommitterEmail: cfg.projectCommitterEmail,
	})
	if keepaliveListener != nil {
		lg.Printf("keepalive provider: %s", cfg.keepaliveProvider)
	}
	if notifierLoop != nil {
		lg.Printf("notifier provider: %s", cfg.notifierProvider)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Init(ctx, func(agent.BackendType) ([]string, error) { return nil, nil }); err != nil {
		lg.Printf("warning: host.Init: %v", err)
	}

	mux := hostmux.New(svc, lg)
	mux.SetAuthToken(cfg.listenAuthToken)
	handler := mux.Handler()
	// Listeners that watch HTTP activity (the exit provider's idle
	// detection) wrap the handler so requests and in-flight streams
	// count as signs of life.
	if tracker, ok := keepaliveListener.(interface {
		TrackHTTP(http.Handler) http.Handler
	}); ok {
		handler = tracker.TrackHTTP(handler)
	}
	srv := &http.Server{Handler: handler}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	serveErr := make(chan error, 1)
	go func() {
		// Print the actual bound address so parents that asked for
		// port :0 (LocalLauncher) can read it from stderr.
		lg.Printf("listening on %s://%s", kind, ln.Addr().String())
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			serveErr <- err
		}
		close(serveErr)
	}()

	select {
	case sig := <-sigCh:
		lg.Printf("received signal %v, shutting down", sig)
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("serve: %w", err)
		}
	}

	shutdownCtx, sc := context.WithTimeout(context.Background(), 5*time.Second)
	defer sc()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		lg.Printf("http shutdown: %v", err)
	}
	svc.Shutdown()
	cancel()
	if sockPath != "" {
		if err := socketutil.RemoveStale(sockPath); err != nil {
			lg.Printf("socket cleanup: %v", err)
		}
	}
	lg.Println("stopped")
	return nil
}

// openListener parses addr and binds the appropriate listener. Returns
// the listener, the scheme ("tcp" or "unix"), and the socket path (for
// unix mode; empty for tcp).
func openListener(addr string) (net.Listener, string, string, error) {
	switch {
	case strings.HasPrefix(addr, "tcp://"):
		host := strings.TrimPrefix(addr, "tcp://")
		ln, err := net.Listen("tcp", host)
		if err != nil {
			return nil, "", "", fmt.Errorf("listen tcp %s: %w", host, err)
		}
		return ln, "tcp", "", nil

	case strings.HasPrefix(addr, "unix://"):
		path := strings.TrimPrefix(addr, "unix://")
		// Remove stale socket file from a prior crashed run. Refuses to
		// touch non-socket files so a bad path cannot clobber user data.
		if err := socketutil.RemoveStale(path); err != nil {
			return nil, "", "", err
		}
		ln, err := net.Listen("unix", path)
		if err != nil {
			return nil, "", "", fmt.Errorf("listen %s: %w", path, err)
		}
		// Restrict the socket to the owner.
		if err := os.Chmod(path, 0o600); err != nil {
			_ = ln.Close()
			return nil, "", "", fmt.Errorf("chmod %s: %w", path, err)
		}
		return ln, "unix", path, nil

	default:
		return nil, "", "", fmt.Errorf("unsupported listen address %q (want tcp:// or unix://)", addr)
	}
}

// parseTemplatesJSON decodes the builtin-template catalog. Empty input
// means no builtin templates; malformed input or entries missing a
// display name or clone URL fail startup — a silently-empty picker is
// exactly the misconfiguration this refuses to hide.
func parseTemplatesJSON(raw string) ([]host.Template, error) {
	if raw == "" {
		return nil, nil
	}
	var templates []host.Template
	if err := json.Unmarshal([]byte(raw), &templates); err != nil {
		return nil, err
	}
	for i, t := range templates {
		if t.DisplayName == "" || t.CloneURL == "" {
			return nil, fmt.Errorf("template %d: display_name and clone_url are required", i)
		}
	}
	return templates, nil
}
