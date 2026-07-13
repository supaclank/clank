package flymachines

import (
	"fmt"
	"time"

	"github.com/acksell/clank/pkg/provisioner"
)

// Defaults for the per-user machine. shared-cpu-8x keeps bundling
// bursts fast; 4GB fits one active worktree (Metro ~1-2GB + a claude
// CLI) with swap as the OOM safety valve — RAM is a per-user knob the
// drift check applies without reprovisioning.
const (
	DefaultGuestCPUKind  = "shared"
	DefaultGuestCPUs     = 8
	DefaultGuestMemoryMB = 4096
	DefaultSwapSizeMB    = 2048
	DefaultVolumeSizeGB  = 10
	// DefaultVolumeSizeLimitGB caps mount auto-extension (grows by
	// DefaultVolumeExtendGB at 80% full). Volumes never shrink, so the
	// cap bounds worst-case storage billing per user.
	DefaultVolumeSizeLimitGB = 50
	DefaultVolumeExtendGB    = 2
	// DefaultSnapshotRetentionDays widens Fly's 5-day default — daily
	// block-level snapshots are the only recovery from an NVMe loss.
	DefaultSnapshotRetentionDays = 14

	DefaultAppNamePrefix    = "clank-u"
	DefaultProvisionTimeout = 5 * time.Minute
)

// HostPort is the one declared service port on the machine —
// clank-host's HTTP API. Flycast routes only declared ports, so all
// gateway traffic (including tunneled preview conns) flows through it.
const HostPort = 8080

// Options configures the Provisioner. APIToken, OrgSlug, Region and
// Image are required — Image in particular has no sane fallback: it
// pins exactly which clank-host + agent-CLI versions every machine
// runs. The operator bakes this image (clank-host + the CLI versions
// it reports via `clank-host print-pins`) and pushes it to a registry
// their Fly org can pull.
type Options struct {
	APIToken string // org-scoped Fly token with app-create rights
	OrgSlug  string
	Region   string

	// Image is the clank-host OCI ref, ideally digest-pinned
	// (ghcr.io/acksell/clank-host-fly@sha256:…). A change here is
	// picked up by the drift check on each user's next EnsureHost.
	Image string

	// GatewayNetwork is the Fly private network the per-app Flycast
	// address is allocated on — the network the gateway dials FROM.
	// Empty means the org's default network (where an app created
	// without an explicit network lives).
	GatewayNetwork string

	// AppNamePrefix namespaces this deployment's apps within the org
	// (prod/dev/test). Defaults to DefaultAppNamePrefix.
	AppNamePrefix string

	GuestCPUKind  string // "" → DefaultGuestCPUKind
	GuestCPUs     int    // 0 → DefaultGuestCPUs
	GuestMemoryMB int    // 0 → DefaultGuestMemoryMB
	SwapSizeMB    int    // 0 → DefaultSwapSizeMB
	VolumeSizeGB  int    // 0 → DefaultVolumeSizeGB

	// ProvisionTimeout caps EnsureHost end-to-end (cold create incl.
	// image pull can take tens of seconds). 0 → DefaultProvisionTimeout.
	ProvisionTimeout time.Duration

	// NotifierWebhookURL / PreviewWebhookURL / GitHubOAuthClientID are
	// forwarded to clank-host via machine env — same contract as the
	// other cloud provisioners. Empty disables each subsystem.
	NotifierWebhookURL  string
	PreviewWebhookURL   string
	GitHubOAuthClientID string

	// Templates is the operator's builtin create-project catalog,
	// forwarded to clank-host as $CLANK_TEMPLATES (marshaled by the
	// provider). Empty means the host serves only the user's own GitHub
	// templates. Part of steady-state machine env so it survives the
	// drift reconcile.
	Templates []provisioner.Template

	// RestoreURLFor, when non-nil and returning non-empty, supplies a
	// one-shot CLANK_RESTORE_URL for a user's FIRST provision (sandbox
	// migration: the entrypoint unpacks the tarball, then a marker file
	// makes it idempotent). Cold-create only, and reconcile preserves
	// exactly this one key — a single named key rather than an
	// arbitrary map so nothing the drift check can't account for ever
	// reaches the machine env.
	RestoreURLFor func(userID string) string
}

// restoreEnvKey is the single one-shot env key. Named once so
// buildMachineConfig (cold create) and oneShotEnv (reconcile) agree.
const restoreEnvKey = "CLANK_RESTORE_URL"

// withDefaults validates required fields and fills zero values.
func (o Options) withDefaults() (Options, error) {
	if o.APIToken == "" {
		return o, fmt.Errorf("flymachines: APIToken is required")
	}
	if o.OrgSlug == "" {
		return o, fmt.Errorf("flymachines: OrgSlug is required")
	}
	if o.Region == "" {
		return o, fmt.Errorf("flymachines: Region is required")
	}
	if o.Image == "" {
		return o, fmt.Errorf("flymachines: Image is required")
	}
	if o.AppNamePrefix == "" {
		o.AppNamePrefix = DefaultAppNamePrefix
	}
	if o.GuestCPUKind == "" {
		o.GuestCPUKind = DefaultGuestCPUKind
	}
	if o.GuestCPUs == 0 {
		o.GuestCPUs = DefaultGuestCPUs
	}
	if o.GuestMemoryMB == 0 {
		o.GuestMemoryMB = DefaultGuestMemoryMB
	}
	if o.SwapSizeMB == 0 {
		o.SwapSizeMB = DefaultSwapSizeMB
	}
	if o.VolumeSizeGB == 0 {
		o.VolumeSizeGB = DefaultVolumeSizeGB
	}
	// The mount's auto-extend ceiling is DefaultVolumeSizeLimitGB; a
	// starting size above it would leave the limit below the volume's
	// own size (rejected config / inert auto-extension). Fail fast
	// rather than ship a broken mount.
	if o.VolumeSizeGB > DefaultVolumeSizeLimitGB {
		return o, fmt.Errorf("flymachines: VolumeSizeGB %d exceeds the auto-extend limit %d", o.VolumeSizeGB, DefaultVolumeSizeLimitGB)
	}
	if o.ProvisionTimeout == 0 {
		o.ProvisionTimeout = DefaultProvisionTimeout
	}
	for i, t := range o.Templates {
		if t.DisplayName == "" || t.CloneURL == "" {
			return o, fmt.Errorf("flymachines: Templates[%d] needs display_name and clone_url", i)
		}
	}
	return o, nil
}
