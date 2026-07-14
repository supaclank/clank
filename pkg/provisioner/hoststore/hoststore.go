// Package hoststore defines the persistence contract used by cloud
// provisioners (flysprites, daytona, …) for tracking the per-(userID, provider)
// host record. Splitting it out lets the laptop daemon back it with
// SQLite and lets external integrators (e.g. multi-tenant cloud control planes) back it with
// Postgres without modifying the provisioners themselves.
//
// Contrary to what the name might suggest it's not the host's internal store,
// it's instead a registry to keep track of hosts (sandboxes/sprites).
package hoststore

import (
	"context"
	"errors"
	"time"
)

// HostStatus is the lifecycle state of a persistent host as the
// provisioner most recently observed it. Values are strings rather than
// an enum so adding a new state (e.g. "archived") doesn't require a
// schema migration.
type HostStatus string

const (
	HostStatusRunning HostStatus = "running"
	HostStatusStopped HostStatus = "stopped"
	HostStatusError   HostStatus = "error"
	// HostStatusProvisioning marks a row claimed by CreateHostIfAbsent
	// before the host's infrastructure exists. The row is a TOKEN CLAIM,
	// not a completion marker — provisioners walk their idempotent
	// create steps regardless of this status, so a crashed provision is
	// resumed by any later caller with no lease/expiry logic.
	HostStatusProvisioning HostStatus = "provisioning"
)

// Host is the persistent record of a user's host across daemon restarts.
// One row per (UserID, Provider) — implementations enforce UNIQUE.
//
// LastURL/LastToken are cache hints, not the source of truth: a stale
// entry is expected after stop/resume and the provisioner refreshes
// them when /status fails.
//
// AuthToken is the clank-host bearer token, baked into the
// sandbox/sprite at create time. Stable across stop/resume — re-read
// on every EnsureHost so the local-side transport stays in sync.
//
// NotifierToken is the per-host bearer token clank-host sends OUTBOUND
// to clankd's /webhooks/notifications endpoint. The dispatcher reverse-
// looks-up by this column to resolve (host_id, user_id). Empty on
// legacy rows; populated lazily on the next cold-create. Counterpart
// to AuthToken — auth_token guards INCOMING traffic into the host,
// notifier_token authenticates OUTGOING traffic from the host.
type Host struct {
	ID            string
	UserID        string
	Provider      string
	ExternalID    string
	Hostname      string
	Status        HostStatus
	LastURL       string
	LastToken     string
	AuthToken     string
	NotifierToken string
	AutoWake      bool
	CreatedAt     time.Time
	UpdatedAt     time.Time

	// ProviderMeta is a small provider-owned key→value bag for resource
	// handles the provider can't derive (e.g. flymachines' server-
	// assigned volume ID). Written ONLY through CASProviderMeta —
	// UpsertHost never touches it — so concurrent provisioner instances
	// can use it as a serialization point. If a second resource kind
	// ever needs relational queries, promote this to a polymorphic
	// host_resources table; until then a JSON column is the whole
	// design. Usage metering must NOT read this — usage ledgers copy
	// identifiers at observation time so billing history survives
	// resource deletion.
	ProviderMeta map[string]string
}

// ErrHostNotFound is returned by GetHostByUser/GetHostByID when no host
// matches. Callers should treat this as a non-error signal to provision.
var ErrHostNotFound = errors.New("host not found")

// HostStore is the persistence contract a provisioner depends on. The
// laptop daemon backs this with SQLite (clank/internal/store); external
// integrators back it with their own database (e.g. Postgres).
//
// GetHostByNotifierToken intentionally lives on the *consumer* side
// (pkg/notify.HostLookup), not here — provisioners don't need to know
// notifier tokens exist, and putting it here would break every existing
// HostStore implementer for no reason.
type HostStore interface {
	GetHostByUser(ctx context.Context, userID, provider string) (Host, error)
	GetHostByID(ctx context.Context, id string) (Host, error)
	UpsertHost(ctx context.Context, h Host) error
	DeleteHostByID(ctx context.Context, id string) error
	DeleteHostByUser(ctx context.Context, userID, provider string) error

	// CreateHostIfAbsent atomically inserts h keyed on UNIQUE(user_id,
	// provider) and returns the row that now exists: (h, true) when this
	// call created it, or (the pre-existing row, false) when another
	// caller — possibly another process — got there first. This is the
	// cross-instance claim that keeps concurrent provisioners deriving
	// identical machine configs: exactly one instance's generated tokens
	// win; everyone else reuses the winner's row. The claimed row is a
	// TOKEN CLAIM, not a completion marker — do not gate provisioning
	// steps on it (a winner crashing mid-provision must be resumable by
	// any later caller).
	CreateHostIfAbsent(ctx context.Context, h Host) (Host, bool, error)

	// CASProviderMeta atomically sets ProviderMeta[key] = newValue iff
	// its current value equals oldValue (empty oldValue = key absent).
	// Returns won=true when the write applied; on won=false, current
	// holds the value that beat us. The serialization primitive for
	// provider resources whose IDs are server-assigned (create-then-
	// claim: losers delete their duplicate and adopt current).
	CASProviderMeta(ctx context.Context, hostID, key, oldValue, newValue string) (won bool, current string, err error)
}
