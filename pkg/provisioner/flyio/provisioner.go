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

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/pkg/provisioner"
	"github.com/acksell/clank/pkg/provisioner/hoststore"
	transportpkg "github.com/acksell/clank/pkg/provisioner/transport"
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

// opencodePath is the canonical opencode location — a symlink the
// install script re-points at bun's global bin on every install.
const opencodePath = "/usr/local/bin/opencode"

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

	// ProvisionTimeout caps how long EnsureHost waits for the sprite
	// to become reachable. Default: 5 minutes.
	ProvisionTimeout time.Duration

	// NotifierWebhookURL, when non-empty, configures clank-host to
	// POST agent-lifecycle notifications (idle, permission, error)
	// back to this URL. The dispatcher at that URL resolves the
	// host's bearer token to its owning user. Empty disables the
	// subsystem — laptop dev without a dispatcher.
	NotifierWebhookURL string

	// GitHubOAuthClientID is the Clank GitHub OAuth App client_id
	// forwarded to clank-host as --github-oauth-client-id. Empty
	// disables GitHub Connect on the provisioned sprite (its status
	// endpoint returns available:false). Supaclank reads it from its
	// own env at startup and passes it here.
	GitHubOAuthClientID string

	// PreviewWebhookURL, when non-empty, is the gateway base for the
	// preview register/revoke webhooks (e.g.
	// "https://api.example.dev/webhooks/preview"), forwarded to clank-host
	// as --preview-webhook-url. The host calls it when it spawns a
	// per-worktree preview dev server so the gateway mints a public token.
	// Empty disables cloud preview registration — servers still spawn but
	// no public URL is minted. clank-host reuses the notifier token to
	// authenticate these calls, so NotifierWebhookURL must also be set.
	PreviewWebhookURL string

	// SDKClient overrides the sprites.Client constructor for tests.
	SDKClient *sprites.Client
}

// Provisioner manages one persistent Sprite per (userID, "flyio").
type Provisioner struct {
	opts   Options
	log    *log.Logger
	client *sprites.Client
	store  hoststore.HostStore

	keyMuMap sync.Mutex
	keyMu    map[string]*sync.Mutex

	cacheMu sync.Mutex
	cache   map[string]*cachedHost
}

type cachedHost struct {
	sprite        *sprites.Sprite
	transport     http.RoundTripper
	hostID        string
	hostname      string
	url           string
	authToken     string
	notifierToken string
}

// hostTokens bundles the two per-sprite credentials passed around
// during provisioning. authToken gates INCOMING traffic to clank-host
// (its --listen-auth-token); notifierToken authenticates OUTGOING
// notification webhooks back to clankd. Both are stable across
// hibernation — re-read from the store row on warm resume.
type hostTokens struct {
	auth     string
	notifier string
}

// New constructs a Provisioner. The HostStore is the persistence
// boundary — laptop daemons pass the SQLite-backed store from
// clank/internal/store; external integrators (e.g. multi-tenant cloud control planes) pass
// a Postgres-backed implementation. See pkg/provisioner/hoststore.
func New(opts Options, st hoststore.HostStore, lg *log.Logger) (*Provisioner, error) {
	if st == nil {
		return nil, fmt.Errorf("flyio provisioner: store is required")
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
	sprite, isNew, tokens, err := p.resolveOrCreate(ctx, userID, spriteName)
	if err != nil {
		return provisioner.HostRef{}, err
	}

	// installAndStart is idempotent — every step probes for its own
	// completion. Always run it so half-provisioned sprites self-heal.
	if err := p.installAndStart(ctx, sprite, tokens); err != nil {
		return provisioner.HostRef{}, err
	}
	_ = isNew

	// Re-read the sprite to pick up the URL populated after public mode.
	// IMPORTANT: re-read by sprite.Name(), not the requested spriteName.
	// resolveOrCreate may return a sprite from a stale hoststore row whose
	// name differs from spriteName (the operator changed prefix). Using
	// spriteName here would 404 even though installAndStart succeeded
	// against the actual sprite.
	actualName := sprite.Name()
	fresh, err := p.client.GetSprite(ctx, actualName)
	if err != nil {
		return provisioner.HostRef{}, fmt.Errorf("get sprite %s: %w", actualName, err)
	}
	if fresh.URL == "" {
		return provisioner.HostRef{}, fmt.Errorf("sprite %s has no public URL after provisioning; check sprites-go SDK behavior", actualName)
	}

	// Pin the bearer to fresh.URL's host so a cross-host redirect
	// can't carry the auth-token to a third-party.
	parsedURL, err := url.Parse(fresh.URL)
	if err != nil {
		return provisioner.HostRef{}, fmt.Errorf("parse sprite URL %q: %w", fresh.URL, err)
	}
	transport := &transportpkg.BearerInjector{Token: tokens.auth, Host: parsedURL.Host}
	hostname := "flyio-" + safeHostnameSuffix(actualName)

	// The Service "started" event only means the process is running;
	// the edge still serves a 404 page until clank-host binds its port.
	if err := waitForSpriteReady(ctx, fresh.URL, transport, p.log); err != nil {
		return provisioner.HostRef{}, fmt.Errorf("sprite %s never reached ready: %w", actualName, err)
	}

	// Persist the actual sprite name, not the requested spriteName.
	// Otherwise a stale row from a previous prefix would keep
	// resolving to the old sprite forever (the bug this commit fixes).
	hostID, err := p.persistRow(ctx, userID, actualName, string(hostname), fresh.URL, tokens, isNew)
	if err != nil {
		return provisioner.HostRef{}, fmt.Errorf("persist host row: %w", err)
	}

	cached := &cachedHost{
		sprite:        fresh,
		transport:     transport,
		hostID:        hostID,
		hostname:      hostname,
		url:           fresh.URL,
		authToken:     tokens.auth,
		notifierToken: tokens.notifier,
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
// a fresh token is minted and threaded back for installAndStart. The
// notifier-token follows the same pattern, with one twist: existing
// rows from before the notifier_token column existed return empty,
// and we lazy-backfill at that point so the next service-recreate
// picks up the new flag.
func (p *Provisioner) resolveOrCreate(ctx context.Context, userID, spriteName string) (*sprites.Sprite, bool, hostTokens, error) {
	row, err := p.store.GetHostByUser(ctx, userID, "flyio")
	if err == nil {
		// If the sprite was deleted out-of-band, clear the row and
		// fall through to recreate.
		sprite, fetchErr := p.client.GetSprite(ctx, row.ExternalID)
		if fetchErr == nil {
			tokens := hostTokens{auth: row.AuthToken, notifier: row.NotifierToken}
			if tokens.notifier == "" {
				// Lazy backfill — existing rows from before the
				// column existed get a token on next EnsureHost,
				// which triggers an args-diff in ensureServiceRunning
				// and forces a service-recreate to push the new flag.
				minted, mintErr := generateNotifierToken()
				if mintErr != nil {
					return nil, false, hostTokens{}, fmt.Errorf("generate notifier-token: %w", mintErr)
				}
				tokens.notifier = minted
				p.log.Printf("flyio provisioner: backfilled notifier_token for existing host %s", row.ID)
			}
			return sprite, false, tokens, nil
		}
		if isNotFound(fetchErr) {
			p.log.Printf("flyio provisioner: sprite %s for user %s not found upstream; recreating", row.ExternalID, userID)
			if delErr := p.store.DeleteHostByUser(ctx, userID, "flyio"); delErr != nil {
				p.log.Printf("flyio provisioner: clear stale row: %v", delErr)
			}
			// fall through
		} else {
			return nil, false, hostTokens{}, fmt.Errorf("get sprite %s: %w", row.ExternalID, fetchErr)
		}
	} else if !errors.Is(err, hoststore.ErrHostNotFound) {
		return nil, false, hostTokens{}, fmt.Errorf("look up host: %w", err)
	}

	// Cold create: mint both tokens now so we can bake them into the
	// Service Args from the first service-create call.
	authToken, err := generateAuthToken()
	if err != nil {
		return nil, false, hostTokens{}, fmt.Errorf("generate auth-token: %w", err)
	}
	notifierToken, err := generateNotifierToken()
	if err != nil {
		return nil, false, hostTokens{}, fmt.Errorf("generate notifier-token: %w", err)
	}

	cfg := &sprites.SpriteConfig{
		Region:    p.opts.Region,
		RamMB:     p.opts.RamMB,
		CPUs:      p.opts.CPUs,
		StorageGB: p.opts.StorageGB,
	}

	sprite, err := p.createSprite(ctx, spriteName, cfg)
	if err != nil {
		return nil, false, hostTokens{}, err
	}
	return sprite, true, hostTokens{auth: authToken, notifier: notifierToken}, nil
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
func (p *Provisioner) installAndStart(ctx context.Context, sprite *sprites.Sprite, tokens hostTokens) error {
	// Wake via HTTP first: the SDK's control-WebSocket pool has a stale-
	// conn race on a freshly-hibernated VM, and an HTTP hit avoids it.
	p.wakeViaHTTP(ctx, sprite)

	binReplaced, err := p.ensureBinaryInstalled(ctx, sprite)
	if err != nil {
		return err
	}
	// Sprites' base image ships Claude/Gemini/Codex but not opencode.
	opencodeReinstalled, err := p.ensureOpenCodeInstalled(ctx, sprite)
	if err != nil {
		return err
	}
	// Force a service recreate when either the clank-host binary OR
	// opencode was just swapped. Binary swap: Linux keeps the old
	// inode in memory (POSIX unlink), so without a restart the old
	// process keeps serving even though the path resolves to the new
	// file. Opencode swap: clank-host's /software-manifest endpoint
	// uses a sync.Once-cached probe (agent.GetSoftwareManifest), so a
	// fresh opencode at /usr/local/bin/opencode is invisible until
	// the process restarts and re-probes. Either way, recreate the
	// service to publish the new state.
	if err := p.ensureServiceRunning(ctx, sprite, tokens, binReplaced || opencodeReinstalled); err != nil {
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
// alive while the path resolves to the new file. Returns replaced=true
// when the binary on disk was rewritten — callers use that to force a
// service restart so the new binary actually runs (the original process
// keeps the old inode otherwise).
func (p *Provisioner) ensureBinaryInstalled(ctx context.Context, sprite *sprites.Sprite) (replaced bool, err error) {
	fsys := sprite.Filesystem()
	wf, ok := fsys.(spriteFSWriter)
	if !ok {
		return false, fmt.Errorf("sprites filesystem does not support WriteFileContext+RemoveContext (SDK API drift)")
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
// Returns replaced=true iff the binary on disk was rewritten so callers
// can force a service restart (Linux keeps the old inode running for
// the original process).
func (p *Provisioner) ensureBinaryInstalledOn(ctx context.Context, stat fs.FS, wf spriteFSWriter, path string, want []byte, wantHashHex string) (replaced bool, err error) {
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
			return false, nil
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
		return false, err
	}

	// Stamp the sidecar so the next EnsureHost can short-circuit on a
	// hash match. Sidecar write failures are logged but not surfaced —
	// worst-case the next run reinstalls the (already-current) binary.
	if err := retryClosedConn(ctx, p.log, func() error {
		return wf.WriteFileContext(ctx, path+hashSidecarSuffix, []byte(wantHashHex), 0o644)
	}); err != nil {
		p.log.Printf("flyio provisioner: stamp hash sidecar: %v (binary still up to date)", err)
	}
	return true, nil
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

// ensureOpenCodeInstalled ensures the sprite has opencode at the
// EXACT version clank pins (agent.PinnedOpencodeVersion). Probe-and-
// upgrade semantics: if the binary is present at the right version,
// fast-path return; otherwise install/replace via bun at the pinned
// version and verify.
//
// Why pin instead of "any opencode": opencode's export/import schema
// is forward-incompatible across minor versions, so a sprite running
// a different opencode than the laptop silently corrupts session
// migrations. Pinning makes the version a deliberate, reviewable
// constant in clank instead of "whatever was latest the day the
// sprite was provisioned." See agent.PinnedOpencodeVersion's docstring.
//
// Returns reinstalled=true when /usr/local/bin/opencode was actually
// swapped (i.e. the install script ran). Callers MUST use this to
// force a clank-host service recreate downstream, otherwise the
// running clank-host's sync.Once-cached /software-manifest probe
// will keep reporting the OLD opencode version forever and the
// laptop's version-match check will reject every push. The cache is
// a process-local optimization in agent/software_manifest.go that
// only invalidates on clank-host restart.
func (p *Provisioner) ensureOpenCodeInstalled(ctx context.Context, sprite *sprites.Sprite) (reinstalled bool, err error) {
	probe := func(probeCtx context.Context) (string, error) {
		var versionOut []byte
		probeErr := retryClosedConn(probeCtx, p.log, func() error {
			cmd := sprite.CommandContext(probeCtx, opencodePath, "--version")
			var runErr error
			versionOut, runErr = cmd.Output()
			return runErr
		})
		return strings.TrimSpace(string(versionOut)), probeErr
	}
	install := func(installCtx context.Context) ([]byte, error) {
		script := strings.ReplaceAll(opencodeInstallScript, "__PINNED_VERSION__", agent.PinnedOpencodeVersion)
		var out []byte
		runErr := retryClosedConn(installCtx, p.log, func() error {
			cmd := sprite.CommandContext(installCtx, "sh", "-c", script)
			var rerr error
			out, rerr = cmd.CombinedOutput()
			return rerr
		})
		return out, runErr
	}
	return p.ensureOpenCodeInstalledOn(ctx, sprite.Name(), probe, sprite.Filesystem(), install)
}

// ensureOpenCodeInstalledOn is the testable core of
// ensureOpenCodeInstalled. The 3-minute install runs ONLY on positive
// evidence that it's needed:
//
//   - the probe succeeded and reported a non-pinned version, or
//   - the probe executed on the sprite and exited non-zero (broken
//     binary, dangling symlink), or
//   - the probe failed at the transport layer AND the filesystem API
//     confirms opencode is absent (fresh sprite).
//
// A transport-level probe failure with opencode present on disk fails
// fast instead: the install would run over the same wedged channel,
// and EnsureHost callers retry with a fresh connection anyway. (Seen
// 2026-07-02: a wake-race probe failure on a freshly-woken sprite
// burned the full 3-minute install deadline inside the request path
// while the pinned version was installed all along.)
func (p *Provisioner) ensureOpenCodeInstalledOn(ctx context.Context, spriteName string, probe func(context.Context) (string, error), statFS fs.FS, install func(context.Context) ([]byte, error)) (reinstalled bool, err error) {
	probeCtx, probeCancel := context.WithTimeout(ctx, 10*time.Second)
	defer probeCancel()
	installed, probeErr := probe(probeCtx)
	switch {
	case probeErr == nil:
		if installed == agent.PinnedOpencodeVersion {
			return false, nil // happy path: present and pinned-version-matched
		}
		p.log.Printf("flyio provisioner: opencode on %s is %q, want %q — reinstalling at pinned version", spriteName, installed, agent.PinnedOpencodeVersion)
	case probeRanOnSprite(probeErr):
		p.log.Printf("flyio provisioner: opencode probe on %s exited non-zero (%v); reinstalling", spriteName, probeErr)
	default:
		if statFS == nil {
			return false, fmt.Errorf("opencode probe on %s did not complete and no filesystem was provided to verify presence: %w", spriteName, probeErr)
		}
		if _, statErr := fs.Stat(statFS, strings.TrimPrefix(opencodePath, "/")); statErr == nil {
			return false, fmt.Errorf("opencode probe on %s did not complete and %s exists — failing fast for a retry on a fresh conn instead of reinstalling: %w", spriteName, opencodePath, probeErr)
		} else if !errors.Is(statErr, fs.ErrNotExist) {
			return false, fmt.Errorf("opencode probe on %s did not complete and the presence check failed (%v): %w", spriteName, statErr, probeErr)
		}
		p.log.Printf("flyio provisioner: opencode absent on %s (probe: %v); installing", spriteName, probeErr)
	}

	installCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	out, runErr := install(installCtx)
	if runErr != nil {
		trimmed := strings.TrimSpace(string(out))
		if len(trimmed) > 8192 {
			trimmed = "..." + trimmed[len(trimmed)-8192:]
		}
		return false, fmt.Errorf("install opencode (sprite=%s): %w\n--- install output ---\n%s\n--- end output ---", spriteName, runErr, trimmed)
	}
	p.log.Printf("flyio provisioner: installed opencode %s on sprite %s", agent.PinnedOpencodeVersion, spriteName)
	return true, nil
}

// probeRanOnSprite reports whether err carries a sprite-side exit
// status — i.e. the exec channel worked end-to-end and the failure is
// evidence about opencode itself, not about the transport.
func probeRanOnSprite(err error) bool {
	var exitErr *sprites.ExitError
	return errors.As(err, &exitErr)
}

// opencodeInstallScript installs opencode at the pinned version and
// atomically points /usr/local/bin/opencode at the newly installed
// binary. Design choices:
//
//   - bun is the SOLE writer. We don't try to coordinate with any
//     opencode the sprite image may have pre-baked at
//     /usr/local/bin/opencode — that path is always treated as a
//     symlink we own and re-point on every run.
//
//   - We trust bun's success report and verify the version of the
//     binary bun produced before swapping the symlink. A stale
//     /usr/local/bin/opencode (e.g. from a previous pin baked into
//     the image) is never read — discovery via candidate paths is
//     gone entirely.
//
//   - Hard fail on any of: bun install error, missing bun-produced
//     binary, version mismatch. The caller would otherwise believe
//     ensureOpenCodeInstalled succeeded while a different opencode
//     is present.
const opencodeInstallScript = `set -e

PINNED="__PINNED_VERSION__"

echo "::: opencode install (target version: $PINNED)"

if ! command -v bun >/dev/null 2>&1; then
  echo "::: ERROR: bun is not on PATH — sprite image is missing the bun runtime" >&2
  exit 1
fi
echo "::: using bun ($(bun --version))"
bun install -g "opencode-ai@$PINNED" 2>&1

# Resolve bun's global bin dir. Order: explicit BUN_INSTALL_BIN env,
# then BUN_INSTALL/bin, then ask bun directly. We never fall back to
# /usr/local/bin/ — that's the SYMLINK target, not a writer.
BUN_BIN_DIR=""
if [ -n "$BUN_INSTALL_BIN" ] && [ -d "$BUN_INSTALL_BIN" ]; then
  BUN_BIN_DIR="$BUN_INSTALL_BIN"
elif [ -n "$BUN_INSTALL" ] && [ -d "$BUN_INSTALL/bin" ]; then
  BUN_BIN_DIR="$BUN_INSTALL/bin"
else
  BUN_BIN_DIR=$(bun pm bin -g 2>/dev/null || true)
fi
BUN_OPENCODE="$BUN_BIN_DIR/opencode"

if [ ! -x "$BUN_OPENCODE" ]; then
  echo "::: ERROR: bun install succeeded but $BUN_OPENCODE is missing or not executable" >&2
  echo "::: BUN_INSTALL=$BUN_INSTALL BUN_INSTALL_BIN=$BUN_INSTALL_BIN" >&2
  echo "::: BUN_BIN_DIR=$BUN_BIN_DIR" >&2
  exit 1
fi

# Strict version check on the bun-produced binary BEFORE touching the
# canonical path. If a registry mirror or cache served us the wrong
# version, fail loud here — never expose a mismatched opencode.
got=$("$BUN_OPENCODE" --version 2>/dev/null | tr -d '[:space:]')
if [ "$got" != "$PINNED" ]; then
  echo "::: ERROR: bun installed $got at $BUN_OPENCODE, expected $PINNED" >&2
  exit 1
fi

# Atomic-ish swap. ln -sf overwrites any existing file or symlink at
# /usr/local/bin/opencode in one step. Idempotent across reruns: the
# symlink ends up pointing at the just-verified bun binary regardless
# of whether the image pre-baked anything there.
ln -sf "$BUN_OPENCODE" /usr/local/bin/opencode

echo "::: done — /usr/local/bin/opencode -> $BUN_OPENCODE (version $PINNED)"
`

// ensureServiceRunning registers the clank-host Service, recreating it
// if the persisted Cmd/Args drifted from what this daemon expects OR
// when forceRecreate is set (the caller just replaced the on-disk
// binary; the existing service is still running the prior inode in
// memory and won't pick up the new endpoints until restart).
//
// Drift detection without forceRecreate also catches the historical
// case where a flag rename would crash-loop the service across the
// hibernate/wake cycle and the edge would serve 404s.
func (p *Provisioner) ensureServiceRunning(ctx context.Context, sprite *sprites.Sprite, tokens hostTokens, forceRecreate bool) error {
	wantReq := buildServiceRequest(tokens, p.opts.NotifierWebhookURL, p.opts.PreviewWebhookURL, p.opts.GitHubOAuthClientID)

	var existing *sprites.ServiceWithState
	var existingErr error
	getErr := retryClosedConn(ctx, p.log, func() error {
		s, err := sprite.GetService(ctx, serviceName)
		existing = s
		existingErr = err
		return err
	})
	if getErr == nil && existing != nil {
		if !forceRecreate && serviceMatches(&existing.Service, wantReq) {
			return nil
		}
		if forceRecreate {
			p.log.Printf("flyio provisioner: service %s binary swapped; restarting", serviceName)
		} else {
			p.log.Printf("flyio provisioner: service %s args drifted; recreating", serviceName)
		}
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
// When webhookURL is empty the notifier flags are omitted entirely
// (laptop-dev / no-dispatcher path); when set, the host POSTs idle /
// permission / error events back to the dispatcher. previewWebhookURL
// conditionally adds --preview-webhook-url so the host registers its
// per-worktree preview servers with the gateway (it reuses the notifier
// token, so it only takes effect alongside the notifier flags).
// githubOAuthClientID likewise conditionally adds --github-oauth-client-id;
// empty leaves GitHub Connect disabled on the sprite.
func buildServiceRequest(tokens hostTokens, webhookURL, previewWebhookURL, githubOAuthClientID string) *sprites.ServiceRequest {
	port := HostPort
	args := []string{
		"--listen", fmt.Sprintf("tcp://[::]:%d", HostPort),
		"--listen-auth-token", tokens.auth,
		// Defeat the sprite's last-consumer hibernation timer while
		// agents are emitting events. See internal/keepalive.
		"--keepalive-provider", "sprites",
	}
	if webhookURL != "" && tokens.notifier != "" {
		args = append(args,
			"--notifier-provider", "webhook",
			"--notifier-webhook-url", webhookURL,
			"--notifier-webhook-token", tokens.notifier,
		)
	}
	if previewWebhookURL != "" && webhookURL != "" && tokens.notifier != "" {
		// clank-host reuses the notifier token (passed above) to auth the
		// preview register/revoke calls, so only the URL is needed here.
		args = append(args, "--preview-webhook-url", previewWebhookURL)
	}
	if githubOAuthClientID != "" {
		args = append(args, "--github-oauth-client-id", githubOAuthClientID)
	}
	return &sprites.ServiceRequest{
		Cmd:      installPath,
		Args:     args,
		HTTPPort: &port,
	}
}

// serviceMatches compares a persisted Service to a fresh request.
// Token values (auth-token, notifier-token) are wildcarded — a token
// rotation alone should not force a recreate.
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

// wildcardedArgs lists the flag names whose value should be ignored
// in argsEquivalent. Keep this synced with buildServiceRequest — adding
// a new bearer-style flag without listing it here would force a service
// recreate on every token rotation.
var wildcardedArgs = map[string]bool{
	"--listen-auth-token":      true,
	"--notifier-webhook-token": true,
}

// argsEquivalent compares two arg slices in order, wildcarding the
// value position immediately after any flag in wildcardedArgs.
func argsEquivalent(have, want []string) bool {
	if len(have) != len(want) {
		return false
	}
	for i := 0; i < len(have); i++ {
		if i > 0 && wildcardedArgs[want[i-1]] {
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
func (p *Provisioner) persistRow(ctx context.Context, userID, externalID, hostname, url string, tokens hostTokens, isNew bool) (string, error) {
	now := time.Now()

	hostID := ""
	if existing, err := p.store.GetHostByUser(ctx, userID, "flyio"); err == nil {
		hostID = existing.ID
	} else if !errors.Is(err, hoststore.ErrHostNotFound) {
		return "", err
	}
	if hostID == "" {
		hostID = newHostID()
	}

	rec := hoststore.Host{
		ID:            hostID,
		UserID:        userID,
		Provider:      "flyio",
		ExternalID:    externalID,
		Hostname:      hostname,
		Status:        hoststore.HostStatusRunning,
		LastURL:       url,
		LastToken:     "", // Sprites have no provider-edge token; leave empty
		AuthToken:     tokens.auth,
		NotifierToken: tokens.notifier,
		AutoWake:      true,
		UpdatedAt:     now,
	}
	if isNew {
		rec.CreatedAt = now
	}
	if err := p.store.UpsertHost(ctx, rec); err != nil {
		return "", err
	}
	return hostID, nil
}

// SuspendHost is a no-op for Sprites. Hibernation is gated sprite-side
// on session-event activity (see internal/keepalive) — clank-host
// renews a Sprites Task lease while events flow and stops renewing
// when they stop, letting the platform's last-consumer timer take
// over. No daemon-side action is needed; we keep the method on the
// Provisioner interface so other backends that lack an in-VM signal
// can hook explicit suspend later.
func (p *Provisioner) SuspendHost(ctx context.Context, hostID string) error {
	row, err := p.store.GetHostByID(ctx, hostID)
	if err != nil {
		return fmt.Errorf("look up host %s: %w", hostID, err)
	}
	p.log.Printf("flyio provisioner: SuspendHost is a no-op for sprite %s (keepalive-gated hibernate)", row.ExternalID)
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

// DestroyHostsByUser destroys the user's flyio sprite, if any. Idempotent:
// returns nil when the user has no row. Force-destroys regardless of session
// state (account erasure must not be blocked by a busy session).
func (p *Provisioner) DestroyHostsByUser(ctx context.Context, userID string) error {
	row, err := p.store.GetHostByUser(ctx, userID, "flyio")
	if errors.Is(err, hoststore.ErrHostNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("look up host for user %s: %w", userID, err)
	}
	return p.DestroyHost(ctx, row.ID)
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

// generateNotifierToken returns a per-host bearer prefixed "clnk_" so
// it's distinguishable from auth-tokens in logs and DB rows. ~192 bits
// of entropy is plenty for an API-key-style credential.
func generateNotifierToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return "clnk_" + base64.RawURLEncoding.EncodeToString(b), nil
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
		strings.Contains(msg, "connection reset") ||
		// sprites-go's exec Wait reports a command whose control conn
		// died before an exit code arrived as exactly "connection
		// closed" — the wake-race symptom on a freshly-woken sprite.
		strings.Contains(msg, "connection closed")
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
