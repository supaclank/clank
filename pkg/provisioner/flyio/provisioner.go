// Package flyio implements provisioner.Provisioner using Fly.io
// Sprites (https://sprites.dev) — one persistent sprite per user.
// The public URL is "public" mode; clank-host's bearer middleware is
// the only auth gate, with the per-sprite token persisted on the host
// row so it survives daemon restarts.
package flyio

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	sprites "github.com/superfly/sprites-go"

	"github.com/acksell/clank/internal/store"
	"github.com/acksell/clank/provisioner"
	transportpkg "github.com/acksell/clank/provisioner/transport"
)

// clankHostHashHex is the SHA-256 of the embedded clank-host binary,
// used as a versioning marker. Same-size builds with different content
// (e.g. a one-byte source change) would silently slip past a size-only
// check; the sidecar file <installPath>.installed-sha256 stores the
// last-installed hash so we can detect those.
var clankHostHashHex = func() string {
	sum := sha256.Sum256(clankHostBinary)
	return hex.EncodeToString(sum[:])
}()

// hashSidecarSuffix is the path suffix for the sidecar marker. Read
// after the size match in ensureBinaryInstalledOn; written after a
// successful binary write.
const hashSidecarSuffix = ".installed-sha256"

// HostPort is clank-host's listen port inside the sprite. We set it
// on Service.HTTPPort explicitly rather than relying on Sprites' default.
const HostPort = 8080

// installPath is /usr/local/bin so the path is on PATH and survives
// sprite hibernation.
const installPath = "/usr/local/bin/clank-host"

// serviceName is stable — reused across restarts so the running
// service auto-resumes from hibernation.
const serviceName = "clank-host"

// defaultSpriteNamePrefix is what the user's sprite is named when
// preferences don't override it; the userID is appended.
const defaultSpriteNamePrefix = "clank-host"

// Options configures the SpritesProvisioner.
type Options struct {
	APIToken         string // SPRITES_TOKEN; required when SDKClient is nil
	OrganizationSlug string // optional; default org used when empty
	Region           string // optional Sprites region

	// SpriteNamePrefix is prepended to the userID. Defaults to
	// "clank-host".
	SpriteNamePrefix string

	RamMB     int // 0 = sprite default
	CPUs      int // 0 = sprite default
	StorageGB int // 0 = sprite default

	// HubBaseURL/HubAuthToken pass through to clank-host so the sprite
	// can clone from the cloud-hub mirror.
	HubBaseURL   string
	HubAuthToken string

	// ProvisionTimeout caps how long EnsureHost waits for the sprite
	// to become reachable. Default: 5 minutes.
	ProvisionTimeout time.Duration

	// SDKClient overrides the sprites.Client constructor for tests.
	SDKClient *sprites.Client
}

// Provisioner manages one persistent Sprite per (userID, "flyio").
type Provisioner struct {
	opts   Options
	log    *log.Logger
	client *sprites.Client
	store  *store.Store

	keyMuMap sync.Mutex
	keyMu    map[string]*sync.Mutex

	cacheMu sync.Mutex
	cache   map[string]*cachedHost
}

type cachedHost struct {
	sprite    *sprites.Sprite
	transport http.RoundTripper
	hostID    string
	hostname  string
	url       string
	authToken string
}

// New constructs a Provisioner.
//
// todo(ae): don't use central store package (un-necessary abstraction), use custom instead.
func New(opts Options, st *store.Store, lg *log.Logger) (*Provisioner, error) {
	if st == nil {
		return nil, fmt.Errorf("flyio provisioner: store is required")
	}
	if opts.HubBaseURL == "" {
		return nil, fmt.Errorf("flyio provisioner: HubBaseURL is required")
	}
	if opts.HubAuthToken == "" {
		return nil, fmt.Errorf("flyio provisioner: HubAuthToken is required")
	}
	if opts.SpriteNamePrefix == "" {
		opts.SpriteNamePrefix = defaultSpriteNamePrefix
	}
	if opts.ProvisionTimeout == 0 {
		opts.ProvisionTimeout = 5 * time.Minute
	}
	if lg == nil {
		lg = log.Default()
	}

	c := opts.SDKClient
	if c == nil {
		if opts.APIToken == "" {
			return nil, fmt.Errorf("flyio provisioner: APIToken is required (or pass an SDKClient for tests)")
		}
		c = sprites.New(opts.APIToken)
	}

	return &Provisioner{
		opts:   opts,
		log:    lg,
		client: c,
		store:  st,
		keyMu:  map[string]*sync.Mutex{},
		cache:  map[string]*cachedHost{},
	}, nil
}

// Stop is a no-op: sprites auto-hibernate natively.
func (p *Provisioner) Stop() {}

// EnsureHost implements provisioner.Provisioner.
//
// Detaches from the caller's cancellation (cold install runs 30–90s,
// far longer than typical TUI request budgets) and bounds work with
// ProvisionTimeout instead. A per-userID mutex serializes concurrent
// callers onto a single in-flight provision.
func (p *Provisioner) EnsureHost(ctx context.Context, userID string) (provisioner.HostRef, error) {
	if userID == "" {
		return provisioner.HostRef{}, fmt.Errorf("flyio provisioner: userID is required")
	}
	mu := p.userMutex(userID)
	mu.Lock()
	defer mu.Unlock()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.opts.ProvisionTimeout)
	defer cancel()

	// Fast path: in-process cache. Sprites' edge auto-wakes a hibernated
	// VM on traffic, so we don't pre-probe.
	if c := p.cacheGet(userID); c != nil {
		return p.refToHost(c), nil
	}

	spriteName := p.spriteNameFor(userID)
	sprite, isNew, authToken, err := p.resolveOrCreate(ctx, userID, spriteName)
	if err != nil {
		return provisioner.HostRef{}, err
	}

	// installAndStart is idempotent — every step probes for its own
	// completion. Always run it so half-provisioned sprites self-heal.
	if err := p.installAndStart(ctx, sprite, authToken); err != nil {
		return provisioner.HostRef{}, err
	}
	_ = isNew

	// Re-read the sprite to pick up the URL populated after public mode.
	fresh, err := p.client.GetSprite(ctx, spriteName)
	if err != nil {
		return provisioner.HostRef{}, fmt.Errorf("get sprite %s: %w", spriteName, err)
	}
	if fresh.URL == "" {
		return provisioner.HostRef{}, fmt.Errorf("sprite %s has no public URL after provisioning; check sprites-go SDK behavior", spriteName)
	}

	// Pin the bearer to fresh.URL's host so a cross-host redirect
	// can't carry the auth-token to a third-party.
	parsedURL, err := url.Parse(fresh.URL)
	if err != nil {
		return provisioner.HostRef{}, fmt.Errorf("parse sprite URL %q: %w", fresh.URL, err)
	}
	transport := &transportpkg.BearerInjector{Token: authToken, Host: parsedURL.Host}
	hostname := "flyio-" + safeHostnameSuffix(spriteName)

	// The Service "started" event only means the process is running;
	// the edge still serves a 404 page until clank-host binds its port.
	if err := waitForSpriteReady(ctx, fresh.URL, transport, p.log); err != nil {
		return provisioner.HostRef{}, fmt.Errorf("sprite %s never reached ready: %w", spriteName, err)
	}

	hostID, err := p.persistRow(ctx, userID, spriteName, string(hostname), fresh.URL, authToken, isNew)
	if err != nil {
		return provisioner.HostRef{}, fmt.Errorf("persist host row: %w", err)
	}

	cached := &cachedHost{
		sprite:    fresh,
		transport: transport,
		hostID:    hostID,
		hostname:  hostname,
		url:       fresh.URL,
		authToken: authToken,
	}
	p.cacheSet(userID, cached)
	return p.refToHost(cached), nil
}

// waitForSpriteReady polls /status until clank-host's mux responds
// (anything other than the Sprites edge 404).
func waitForSpriteReady(ctx context.Context, baseURL string, transport http.RoundTripper, _ *log.Logger) error {
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()

	client := &http.Client{Transport: transport}
	url := strings.TrimRight(baseURL, "/") + "/status"
	var (
		lastBody   string
		lastStatus int
	)
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		req, _ := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)
		resp, err := client.Do(req)
		cancel()
		if err == nil {
			lastStatus = resp.StatusCode
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
			resp.Body.Close()
			lastBody = string(body)
			if !isSpritesEdge404(resp.StatusCode, body) {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("ctx done (last status=%d, body snippet=%q)", lastStatus, snippet(lastBody))
		case <-deadline.C:
			return fmt.Errorf("timed out waiting for sprite (last status=%d, body snippet=%q)", lastStatus, snippet(lastBody))
		case <-t.C:
		}
	}
}

// isSpritesEdge404 distinguishes the edge "no service bound" page from
// a real host 404 by the title string the edge always emits.
func isSpritesEdge404(status int, body []byte) bool {
	if status != http.StatusNotFound {
		return false
	}
	return bytes.Contains(body, []byte("<title>404 | Sprites"))
}

// snippet shortens a body for inclusion in a timeout error message.
func snippet(s string) string {
	const max = 120
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

func (p *Provisioner) refToHost(c *cachedHost) provisioner.HostRef {
	return provisioner.HostRef{
		HostID:    c.hostID,
		URL:       c.url,
		Transport: c.transport,
		AuthToken: c.authToken,
		AutoWake:  true, // Sprites edge wakes on traffic
		Hostname:  c.hostname,
	}
}

// resolveOrCreate returns the user's sprite, creating it if absent.
// On reuse the auth-token is read from the store row; on cold create
// a fresh token is minted and threaded back for installAndStart.
func (p *Provisioner) resolveOrCreate(ctx context.Context, userID, spriteName string) (*sprites.Sprite, bool, string, error) {
	row, err := p.store.GetHostByUser(ctx, userID, "flyio")
	if err == nil {
		// If the sprite was deleted out-of-band, clear the row and
		// fall through to recreate.
		sprite, fetchErr := p.client.GetSprite(ctx, row.ExternalID)
		if fetchErr == nil {
			return sprite, false, row.AuthToken, nil
		}
		if isNotFound(fetchErr) {
			p.log.Printf("flyio provisioner: sprite %s for user %s not found upstream; recreating", row.ExternalID, userID)
			if delErr := p.store.DeleteHostByUser(ctx, userID, "flyio"); delErr != nil {
				p.log.Printf("flyio provisioner: clear stale row: %v", delErr)
			}
			// fall through
		} else {
			return nil, false, "", fmt.Errorf("get sprite %s: %w", row.ExternalID, fetchErr)
		}
	} else if !errors.Is(err, store.ErrHostNotFound) {
		return nil, false, "", fmt.Errorf("look up host: %w", err)
	}

	// Cold create: mint the auth-token now so we can bake it into the
	// Service Args.
	authToken, err := generateAuthToken()
	if err != nil {
		return nil, false, "", fmt.Errorf("generate auth-token: %w", err)
	}

	cfg := &sprites.SpriteConfig{
		Region:    p.opts.Region,
		RamMB:     p.opts.RamMB,
		CPUs:      p.opts.CPUs,
		StorageGB: p.opts.StorageGB,
	}

	sprite, err := p.createSprite(ctx, spriteName, cfg)
	if err != nil {
		return nil, false, "", err
	}
	return sprite, true, authToken, nil
}

// createSprite chooses the org-scoped or default-org variant.
func (p *Provisioner) createSprite(ctx context.Context, name string, cfg *sprites.SpriteConfig) (*sprites.Sprite, error) {
	startCreate := time.Now()
	var (
		sprite *sprites.Sprite
		err    error
	)
	if p.opts.OrganizationSlug != "" {
		// SDK's OrganizationInfo uses Name, not Slug — Sprites
		// currently uses the same identifier for both.
		sprite, err = p.client.CreateSpriteWithOrg(ctx, name, cfg, &sprites.OrganizationInfo{Name: p.opts.OrganizationSlug}, nil)
	} else {
		sprite, err = p.client.CreateSprite(ctx, name, cfg)
	}
	if err != nil {
		return nil, fmt.Errorf("create sprite %s: %w", name, err)
	}
	p.log.Printf("flyio provisioner: sprite %s created in %s", name, time.Since(startCreate).Round(time.Millisecond))
	return sprite, nil
}

// installAndStart pushes the binary, installs the opencode runtime,
// registers the service, and opens the URL. Idempotent end-to-end so
// half-provisioned sprites self-heal on the next daemon start.
func (p *Provisioner) installAndStart(ctx context.Context, sprite *sprites.Sprite, authToken string) error {
	// Wake via HTTP first: the SDK's control-WebSocket pool has a stale-
	// conn race on a freshly-hibernated VM, and an HTTP hit avoids it.
	p.wakeViaHTTP(ctx, sprite)

	if err := p.ensureBinaryInstalled(ctx, sprite); err != nil {
		return err
	}
	// Sprites' base image ships Claude/Gemini/Codex but not opencode.
	if err := p.ensureOpenCodeInstalled(ctx, sprite); err != nil {
		return err
	}
	if err := p.ensureServiceRunning(ctx, sprite, authToken); err != nil {
		return err
	}
	// Re-apply on every run so a manually-disabled URL re-opens.
	if err := sprite.UpdateURLSettings(ctx, &sprites.URLSettings{Auth: "public"}); err != nil {
		return fmt.Errorf("update sprite URL to public: %w", err)
	}
	return nil
}

// ensureBinaryInstalled writes the embedded clank-host binary, skipping
// the ~17MB upload when a same-size file is already present.
//
// Replacement uses unlink-then-write: Linux returns ETXTBSY on writing
// to a running executable, and POSIX unlink keeps the running inode
// alive while the path resolves to the new file.
func (p *Provisioner) ensureBinaryInstalled(ctx context.Context, sprite *sprites.Sprite) error {
	fsys := sprite.Filesystem()
	wf, ok := fsys.(spriteFSWriter)
	if !ok {
		return fmt.Errorf("sprites filesystem does not support WriteFileContext+RemoveContext (SDK API drift)")
	}
	return p.ensureBinaryInstalledOn(ctx, fsys, wf, installPath, clankHostBinary, clankHostHashHex)
}

// spriteFSWriter is the SDK filesystem subset needed for atomic
// binary replacement. Stubbed in tests.
type spriteFSWriter interface {
	WriteFileContext(ctx context.Context, name string, data []byte, perm fs.FileMode) error
	RemoveContext(ctx context.Context, name string) error
}

// ensureBinaryInstalledOn is the testable core of ensureBinaryInstalled.
// wantHashHex is the sha256 of want. After a size match, the sidecar
// file at path+hashSidecarSuffix is also compared so that two builds
// with the same byte length but different content trigger a reinstall.
func (p *Provisioner) ensureBinaryInstalledOn(ctx context.Context, stat fs.FS, wf spriteFSWriter, path string, want []byte, wantHashHex string) error {
	var info fs.FileInfo
	statErr := retryClosedConn(ctx, p.log, func() error {
		var err error
		info, err = fs.Stat(stat, strings.TrimPrefix(path, "/"))
		return err
	})
	if statErr == nil && info.Size() == int64(len(want)) {
		// Size matches — verify the sidecar before declaring done.
		// Missing/mismatched sidecar means the size collision is
		// hiding a real version skew, so fall through to reinstall.
		if got, ok := readSidecar(stat, path+hashSidecarSuffix); ok && got == wantHashHex {
			return nil
		}
		p.log.Printf("flyio provisioner: clank-host size matches but hash sidecar missing/stale; reinstalling")
	} else if statErr == nil {
		p.log.Printf("flyio provisioner: clank-host binary size mismatch (have %d, want %d); replacing", info.Size(), len(want))
	} else {
		p.log.Printf("flyio provisioner: clank-host not present on sprite (%v); installing (%d bytes)", statErr, len(want))
	}

	// Best-effort unlink before write; ENOENT is fine.
	_ = retryClosedConn(ctx, p.log, func() error {
		err := wf.RemoveContext(ctx, path)
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		p.log.Printf("flyio provisioner: pre-write remove of %s: %v (continuing)", path, err)
		return nil
	})

	if err := retryClosedConn(ctx, p.log, func() error {
		if err := wf.WriteFileContext(ctx, path, want, 0o755); err != nil {
			return fmt.Errorf("install clank-host binary: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	// Stamp the sidecar so the next EnsureHost can short-circuit on a
	// hash match. Sidecar write failures are logged but not surfaced —
	// worst-case the next run reinstalls the (already-current) binary.
	if err := retryClosedConn(ctx, p.log, func() error {
		return wf.WriteFileContext(ctx, path+hashSidecarSuffix, []byte(wantHashHex), 0o644)
	}); err != nil {
		p.log.Printf("flyio provisioner: stamp hash sidecar: %v (binary still up to date)", err)
	}
	return nil
}

// readSidecar reads the hash sidecar file from the sprite filesystem.
// Returns (hex, true) on a successful read; (_, false) on any error
// (missing, permission, decode) so the caller can treat it as "stale".
func readSidecar(stat fs.FS, sidecarPath string) (string, bool) {
	f, err := stat.Open(strings.TrimPrefix(sidecarPath, "/"))
	if err != nil {
		return "", false
	}
	defer f.Close()
	// Sidecar is a 64-char hex string; cap the read defensively.
	buf, err := io.ReadAll(io.LimitReader(f, 256))
	if err != nil {
		return "", false
	}
	got := strings.TrimSpace(string(buf))
	if len(got) != hex.EncodedLen(sha256.Size) {
		return "", false
	}
	return got, true
}

// ensureOpenCodeInstalled probes for opencode at /usr/local/bin and
// installs it (via bun, npm fallback) when missing. The probe is the
// fast-path on every cold cache miss; first-time install is 30-90s.
//
// We symlink the bun-shipped JS wrapper rather than the platform
// binary directly — the platform binary's dynamic linker may be
// missing (e.g. musl loader on a glibc sprite). Each candidate path
// is verified with --version before we accept it.
func (p *Provisioner) ensureOpenCodeInstalled(ctx context.Context, sprite *sprites.Sprite) error {
	probeCtx, probeCancel := context.WithTimeout(ctx, 10*time.Second)
	defer probeCancel()
	probeErr := retryClosedConn(probeCtx, p.log, func() error {
		return sprite.CommandContext(probeCtx, "/usr/local/bin/opencode", "--version").Run()
	})
	if probeErr == nil {
		return nil
	}

	installCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	const script = `set -e

echo "::: opencode install"

# Pick a package manager. Sprites have bun for the pre-installed
# AI CLIs, so it should always be present.
PM=""
if command -v bun >/dev/null 2>&1; then
  PM="bun"
  echo "::: using bun ($(bun --version))"
  bun install -g opencode-ai 2>&1
elif command -v npm >/dev/null 2>&1; then
  PM="npm"
  echo "::: using npm ($(npm --version))"
  npm install -g opencode-ai 2>&1
else
  echo "::: ERROR: neither bun nor npm available" >&2
  exit 1
fi

# Try candidate paths in priority order. Wrappers FIRST, package
# binaries LAST — and only accept a candidate that actually executes.
echo "::: locating opencode"

# Bun's global bin dir. Resolved at runtime since the install path
# is sprites-specific (we saw /.sprite/languages/bun/install/global/...).
BUN_BIN=""
if [ "$PM" = "bun" ]; then
  if [ -n "$BUN_INSTALL_BIN" ] && [ -d "$BUN_INSTALL_BIN" ]; then
    BUN_BIN="$BUN_INSTALL_BIN"
  elif [ -n "$BUN_INSTALL" ] && [ -d "$BUN_INSTALL/bin" ]; then
    BUN_BIN="$BUN_INSTALL/bin"
  else
    # Walk up from the bun executable to find its prefix.
    BUN_BIN=$(dirname "$(command -v bun)")
  fi
fi

CANDIDATES="
/usr/local/bin/opencode
$BUN_BIN/opencode
$HOME/.bun/bin/opencode
/root/.bun/bin/opencode
/.bun/bin/opencode
/usr/local/bin/opencode
"

ACTUAL=""
for cand in $CANDIDATES; do
  [ -z "$cand" ] && continue
  if [ -x "$cand" ] && "$cand" --version >/dev/null 2>&1; then
    ACTUAL="$cand"
    echo "::: found working binary at $cand"
    break
  elif [ -x "$cand" ]; then
    echo "::: $cand exists but failed --version"
  fi
done

# Last-resort: filesystem scan, but exclude node_modules so we never
# pick up a platform-specific package binary that needs a missing
# dynamic linker.
if [ -z "$ACTUAL" ]; then
  echo "::: scanning filesystem (excluding node_modules)"
  for cand in $(find / -name 'opencode' -type f -executable -not -path '*/node_modules/*' 2>/dev/null); do
    if "$cand" --version >/dev/null 2>&1; then
      ACTUAL="$cand"
      echo "::: scan found working binary at $cand"
      break
    fi
  done
fi

if [ -z "$ACTUAL" ]; then
  echo "::: ERROR: no working opencode binary found after install" >&2
  echo "::: PATH=$PATH" >&2
  echo "::: BUN_INSTALL=$BUN_INSTALL BUN_INSTALL_BIN=$BUN_INSTALL_BIN" >&2
  echo "::: candidates tried:" >&2
  for cand in $CANDIDATES; do
    [ -z "$cand" ] && continue
    if [ -e "$cand" ]; then
      echo ":::   $cand exists; --version output:" >&2
      "$cand" --version 2>&1 | sed 's/^/:::     /' >&2 || true
    fi
  done >&2
  exit 1
fi

# Symlink to /usr/local/bin so the service's PATH can resolve it.
if [ "$ACTUAL" != "/usr/local/bin/opencode" ]; then
  echo "::: symlinking $ACTUAL -> /usr/local/bin/opencode"
  ln -sf "$ACTUAL" /usr/local/bin/opencode
fi

# Verify the canonical path one more time end-to-end.
echo "::: verifying /usr/local/bin/opencode"
/usr/local/bin/opencode --version
echo "::: done"
`
	var out []byte
	err := retryClosedConn(installCtx, p.log, func() error {
		cmd := sprite.CommandContext(installCtx, "sh", "-c", script)
		var runErr error
		out, runErr = cmd.CombinedOutput()
		return runErr
	})
	if err != nil {
		trimmed := strings.TrimSpace(string(out))
		if len(trimmed) > 8192 {
			trimmed = "..." + trimmed[len(trimmed)-8192:]
		}
		return fmt.Errorf("install opencode (sprite=%s): %w\n--- install output ---\n%s\n--- end output ---", sprite.Name(), err, trimmed)
	}
	p.log.Printf("flyio provisioner: installed opencode on sprite %s", sprite.Name())
	return nil
}

// ensureServiceRunning registers the clank-host Service, recreating it
// if the persisted Cmd/Args drifted from what this daemon expects.
// Without drift detection, a flag rename would crash-loop the service
// across the hibernate/wake cycle and the edge would serve 404s.
func (p *Provisioner) ensureServiceRunning(ctx context.Context, sprite *sprites.Sprite, authToken string) error {
	wantReq := buildServiceRequest(authToken)

	var existing *sprites.ServiceWithState
	var existingErr error
	getErr := retryClosedConn(ctx, p.log, func() error {
		s, err := sprite.GetService(ctx, serviceName)
		existing = s
		existingErr = err
		return err
	})
	if getErr == nil && existing != nil {
		if serviceMatches(&existing.Service, wantReq) {
			return nil
		}
		p.log.Printf("flyio provisioner: service %s args drifted; recreating", serviceName)
		if err := retryClosedConn(ctx, p.log, func() error {
			return sprite.DeleteService(ctx, serviceName)
		}); err != nil {
			return fmt.Errorf("delete drifted clank-host service: %w", err)
		}
	} else if getErr != nil && !isNotFound(existingErr) {
		return fmt.Errorf("get clank-host service: %w", getErr)
	}

	var stream *sprites.ServiceStream
	if err := retryClosedConn(ctx, p.log, func() error {
		var err error
		stream, err = sprite.CreateService(ctx, serviceName, wantReq)
		return err
	}); err != nil {
		return fmt.Errorf("create clank-host service: %w", err)
	}
	if err := waitForServiceStarted(stream); err != nil {
		return fmt.Errorf("wait for clank-host service started: %w", err)
	}
	return nil
}

// buildServiceRequest is the canonical Service shape this daemon
// expects, used both to create and to compare against a persisted one.
func buildServiceRequest(authToken string) *sprites.ServiceRequest {
	port := HostPort
	return &sprites.ServiceRequest{
		Cmd: installPath,
		Args: []string{
			"--listen", fmt.Sprintf("tcp://[::]:%d", HostPort),
			"--listen-auth-token", authToken,
		},
		HTTPPort: &port,
	}
}

// serviceMatches compares a persisted Service to a fresh request.
// The auth-token value is wildcarded — a token rotation alone should
// not force a recreate.
func serviceMatches(have *sprites.Service, want *sprites.ServiceRequest) bool {
	if have.Cmd != want.Cmd {
		return false
	}
	if (have.HTTPPort == nil) != (want.HTTPPort == nil) {
		return false
	}
	if have.HTTPPort != nil && want.HTTPPort != nil && *have.HTTPPort != *want.HTTPPort {
		return false
	}
	return argsEquivalent(have.Args, want.Args)
}

// argsEquivalent compares two arg slices in order, wildcarding the
// value after --listen-auth-token.
func argsEquivalent(have, want []string) bool {
	if len(have) != len(want) {
		return false
	}
	for i := 0; i < len(have); i++ {
		if i > 0 && want[i-1] == "--listen-auth-token" {
			continue
		}
		if have[i] != want[i] {
			return false
		}
	}
	return true
}

// waitForServiceStarted drains a Service log stream until "started"
// or an error/exit event arrives.
func waitForServiceStarted(stream *sprites.ServiceStream) error {
	defer stream.Close()
	for {
		evt, err := stream.Next()
		if err != nil {
			return err
		}
		if evt == nil {
			return fmt.Errorf("service stream closed before reaching 'started' state")
		}
		switch evt.Type {
		case "started":
			return nil
		case "error":
			return fmt.Errorf("service start failed: %s", evt.Data)
		case "exit":
			code := -1
			if evt.ExitCode != nil {
				code = *evt.ExitCode
			}
			return fmt.Errorf("service exited (code=%d) before reaching 'started' state", code)
		}
	}
}

// persistRow upserts the host row.
func (p *Provisioner) persistRow(ctx context.Context, userID, externalID, hostname, url, authToken string, isNew bool) (string, error) {
	now := time.Now()

	hostID := ""
	if existing, err := p.store.GetHostByUser(ctx, userID, "flyio"); err == nil {
		hostID = existing.ID
	} else if !errors.Is(err, store.ErrHostNotFound) {
		return "", err
	}
	if hostID == "" {
		hostID = newHostID()
	}

	rec := store.Host{
		ID:         hostID,
		UserID:     userID,
		Provider:   "flyio",
		ExternalID: externalID,
		Hostname:   hostname,
		Status:     store.HostStatusRunning,
		LastURL:    url,
		LastToken:  "", // Sprites have no provider-edge token; leave empty
		AuthToken:  authToken,
		AutoWake:   true,
		UpdatedAt:  now,
	}
	if isNew {
		rec.CreatedAt = now
	}
	if err := p.store.UpsertHost(ctx, rec); err != nil {
		return "", err
	}
	return hostID, nil
}

// SuspendHost is a near-no-op: Sprites auto-hibernate on idle, so
// explicit suspend isn't needed for cost control.
func (p *Provisioner) SuspendHost(ctx context.Context, hostID string) error {
	row, err := p.store.GetHostByID(ctx, hostID)
	if err != nil {
		return fmt.Errorf("look up host %s: %w", hostID, err)
	}
	p.log.Printf("flyio provisioner: SuspendHost is a no-op for sprite %s (auto-hibernate)", row.ExternalID)
	return nil
}

// DestroyHost permanently deletes the sprite and the store row.
func (p *Provisioner) DestroyHost(ctx context.Context, hostID string) error {
	row, err := p.store.GetHostByID(ctx, hostID)
	if err != nil {
		return fmt.Errorf("look up host %s: %w", hostID, err)
	}
	if err := p.client.DeleteSprite(ctx, row.ExternalID); err != nil {
		if !isNotFound(err) {
			return fmt.Errorf("delete sprite %s: %w", row.ExternalID, err)
		}
	}
	if err := p.store.DeleteHostByID(ctx, hostID); err != nil {
		return fmt.Errorf("delete host row %s: %w", hostID, err)
	}
	p.cacheDrop(row.UserID)
	return nil
}

// userMutex returns the per-userID mutex, creating it on first use.
func (p *Provisioner) userMutex(userID string) *sync.Mutex {
	p.keyMuMap.Lock()
	defer p.keyMuMap.Unlock()
	if mu, ok := p.keyMu[userID]; ok {
		return mu
	}
	mu := &sync.Mutex{}
	p.keyMu[userID] = mu
	return mu
}

func (p *Provisioner) cacheGet(userID string) *cachedHost {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	return p.cache[userID]
}

func (p *Provisioner) cacheSet(userID string, c *cachedHost) {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	p.cache[userID] = c
}

func (p *Provisioner) cacheDrop(userID string) {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	delete(p.cache, userID)
}

// spriteNameFor derives a sprite name from a userID, sanitized for
// Sprites' name validation. When the sanitization is lossy (input
// contains characters Sprites doesn't allow, or the input is symbol-
// only), a short hash of the original userID is appended so distinct
// inputs always produce distinct sprite names.
func (p *Provisioner) spriteNameFor(userID string) string {
	suffix := safeSpriteSuffix(userID)
	if suffix == "" || suffix != strings.ToLower(userID) {
		// Lossy sanitization (or empty after strip): pin uniqueness
		// with a hash of the raw userID.
		sum := sha256.Sum256([]byte(userID))
		hashFrag := hex.EncodeToString(sum[:3])
		if suffix == "" {
			suffix = hashFrag
		} else {
			suffix = suffix + "-" + hashFrag
		}
	}
	return p.opts.SpriteNamePrefix + "-" + suffix
}

// generateAuthToken returns ~256 bits of URL-safe random.
func generateAuthToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// newHostID mints a ULID for the store row.
func newHostID() string {
	return ulid.Make().String()
}

// safeSpriteSuffix keeps only the lowercase alphanumeric + hyphen
// characters Sprites allows in a name.
func safeSpriteSuffix(userID string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(userID) {
		switch {
		case r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '-':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// safeHostnameSuffix returns the trailing segment of a sprite name,
// capped at 12 chars to match the Daytona naming convention.
func safeHostnameSuffix(spriteName string) string {
	if i := strings.LastIndex(spriteName, "-"); i >= 0 {
		spriteName = spriteName[i+1:]
	}
	if len(spriteName) > 12 {
		spriteName = spriteName[:12]
	}
	return spriteName
}

// isNotFound matches a 404 from sprites-go via string comparison —
// the pinned SDK doesn't expose a typed not-found error.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "404")
}

// isClosedConnErr matches the SDK's stale-control-WebSocket symptoms.
// The pool can hand back a checked-out conn before its readloop marks
// it closed; retrying gives the SDK time to evict and redial.
func isClosedConnErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "use of closed network connection") ||
		strings.Contains(msg, "websocket: close") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "connection reset")
}

// retryClosedConn runs fn up to 4 times, retrying with 200ms/600ms/
// 1.5s/3s backoff on isClosedConnErr; other errors return immediately.
func retryClosedConn(ctx context.Context, lg *log.Logger, fn func() error) error {
	delays := []time.Duration{200 * time.Millisecond, 600 * time.Millisecond, 1500 * time.Millisecond, 3 * time.Second}
	var lastErr error
	for attempt, delay := range delays {
		err := fn()
		if err == nil {
			return nil
		}
		lastErr = err
		if !isClosedConnErr(err) {
			return err
		}
		if lg != nil {
			lg.Printf("flyio provisioner: control conn closed (attempt %d/%d): %v; retrying in %s", attempt+1, len(delays), err, delay)
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return fmt.Errorf("retry canceled: %w (last error: %v)", ctx.Err(), lastErr)
		}
	}
	return fmt.Errorf("after %d retries, control connection still failing: %w", len(delays), lastErr)
}

// wakeViaHTTP nudges the edge to wake the sprite without touching
// the control-WebSocket pool (which has a stale-conn race on a
// freshly-hibernated VM). Best-effort.
func (p *Provisioner) wakeViaHTTP(ctx context.Context, sprite *sprites.Sprite) {
	if sprite.URL == "" {
		return
	}
	wakeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(wakeCtx, "GET", sprite.URL+"/", nil)
	if err != nil {
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		p.log.Printf("flyio provisioner: wake %s via HTTP: %v (continuing)", sprite.Name(), err)
		return
	}
	resp.Body.Close()
}
