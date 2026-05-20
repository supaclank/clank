package flyio

import (
	"testing"

	sprites "github.com/superfly/sprites-go"
)

func intPtr(i int) *int { return &i }

// TestServiceMatches_HappyPath pins the equivalence check we use
// to skip a no-op recreate when the persisted service already
// matches what we'd create now. Without it the provisioner used to
// always trust an existing service no matter how stale its args.
func TestServiceMatches_HappyPath(t *testing.T) {
	t.Parallel()
	want := buildServiceRequest("tok-abc")
	have := &sprites.Service{
		Name:     serviceName,
		Cmd:      want.Cmd,
		Args:     append([]string(nil), want.Args...),
		HTTPPort: intPtr(*want.HTTPPort),
	}
	if !serviceMatches(have, want) {
		t.Fatalf("identical request should match")
	}
}

// TestServiceMatches_DriftedArgsForceRecreate is the headline
// regression: a sprite created by an older clank daemon had a
// ServiceRequest with --git-sync-source/--git-sync-token in its
// Args. After PR 3 dropped those flags from clank-host, the new
// binary refuses to start with "flag provided but not defined" —
// the service crash-loops and the sprite serves 404. Detect the
// drift and force a recreate.
func TestServiceMatches_DriftedArgsForceRecreate(t *testing.T) {
	t.Parallel()
	want := buildServiceRequest("tok")
	have := &sprites.Service{
		Cmd: installPath,
		Args: []string{
			"--listen", "tcp://[::]:8080",
			"--listen-auth-token", "tok",
			"--git-sync-source", "https://stale-hub.example.com",
			"--git-sync-token", "stale-hub-token",
		},
		HTTPPort: intPtr(*want.HTTPPort),
	}
	if serviceMatches(have, want) {
		t.Error("drifted args should NOT match (would skip needed recreate)")
	}
}

// TestServiceMatches_AuthTokenIsWildcarded — a fresh daemon mints
// a new auth token on every cold start, but if the old service is
// still functionally compatible we shouldn't recreate just for the
// rotation. Delete-and-recreate costs ~5–10s of provisioning and
// briefly orphans every session.
func TestServiceMatches_AuthTokenIsWildcarded(t *testing.T) {
	t.Parallel()
	want := buildServiceRequest("new-token")
	// Mirror want.Args exactly, swapping only the auth-token value.
	// Building from want (rather than hard-coding) keeps the test
	// resilient to args additions.
	haveArgs := append([]string(nil), want.Args...)
	for i := 0; i < len(haveArgs)-1; i++ {
		if haveArgs[i] == "--listen-auth-token" {
			haveArgs[i+1] = "old-token"
		}
	}
	have := &sprites.Service{
		Cmd:      want.Cmd,
		Args:     haveArgs,
		HTTPPort: intPtr(*want.HTTPPort),
	}
	if !serviceMatches(have, want) {
		t.Errorf("auth-token-only delta should be tolerated; have=%v want=%v", have.Args, want.Args)
	}
}

// TestServiceMatches_CmdMismatchForceRecreate — a binary path move
// (unlikely but possible across daemon upgrades) must trigger a
// recreate. The sprite would otherwise keep exec'ing the old path.
func TestServiceMatches_CmdMismatchForceRecreate(t *testing.T) {
	t.Parallel()
	want := buildServiceRequest("tok")
	have := &sprites.Service{
		Cmd:      "/usr/bin/clank-host", // moved
		Args:     append([]string(nil), want.Args...),
		HTTPPort: intPtr(*want.HTTPPort),
	}
	if serviceMatches(have, want) {
		t.Error("Cmd mismatch should trigger recreate")
	}
}

// TestServiceMatches_PortMismatchForceRecreate covers the analogous
// case for HTTP port — a future bump from 8080 → 7878 (or whatever)
// must trigger recreate so the sprite's edge routes correctly.
func TestServiceMatches_PortMismatchForceRecreate(t *testing.T) {
	t.Parallel()
	want := buildServiceRequest("tok")
	other := *want.HTTPPort + 1
	have := &sprites.Service{
		Cmd:      want.Cmd,
		Args:     append([]string(nil), want.Args...),
		HTTPPort: &other,
	}
	if serviceMatches(have, want) {
		t.Error("HTTPPort mismatch should trigger recreate")
	}
}

// TestBuildServiceRequest_PassesSpritesKeepalive pins that clank-host
// is told to use the Sprites keepalive provider. Without it the sprite
// hibernates on the next last-consumer-timer expiry, killing every
// running agent the moment all SSE clients disconnect. See
// internal/keepalive.
func TestBuildServiceRequest_PassesSpritesKeepalive(t *testing.T) {
	t.Parallel()
	req := buildServiceRequest("tok")
	var hasFlag, hasValue bool
	for i := 0; i < len(req.Args)-1; i++ {
		if req.Args[i] == "--keepalive-provider" {
			hasFlag = true
			if req.Args[i+1] == "sprites" {
				hasValue = true
			}
		}
	}
	if !hasFlag {
		t.Errorf("--keepalive-provider missing from %v", req.Args)
	}
	if !hasValue {
		t.Errorf("--keepalive-provider must be 'sprites'; got %v", req.Args)
	}
}

// TestServiceMatches_LegacyServiceWithoutKeepaliveForcesRecreate pins
// the upgrade path: an existing sprite created before keepalive was
// wired should be detected as drift so the new provider arg is
// applied. Otherwise the bug fix doesn't reach already-provisioned
// sprites until a manual recreate.
func TestServiceMatches_LegacyServiceWithoutKeepaliveForcesRecreate(t *testing.T) {
	t.Parallel()
	want := buildServiceRequest("tok")
	have := &sprites.Service{
		Cmd: want.Cmd,
		Args: []string{
			"--listen", "tcp://[::]:8080",
			"--listen-auth-token", "tok",
		},
		HTTPPort: intPtr(*want.HTTPPort),
	}
	if serviceMatches(have, want) {
		t.Error("legacy args without --keepalive-provider should NOT match (needed recreate would be skipped)")
	}
}

func TestBuildServiceRequest_OmitsRemovedGitSyncFlags(t *testing.T) {
	t.Parallel()
	req := buildServiceRequest("tok")
	for _, a := range req.Args {
		if a == "--git-sync-source" || a == "--git-sync-token" {
			t.Errorf("buildServiceRequest emits removed flag %q (PR 3 deleted it from clank-host; kept here would crash-loop the sprite)", a)
		}
	}
	// And the listen flags should still be there.
	hasListen, hasAuth := false, false
	for _, a := range req.Args {
		if a == "--listen" {
			hasListen = true
		}
		if a == "--listen-auth-token" {
			hasAuth = true
		}
	}
	if !hasListen || !hasAuth {
		t.Errorf("missing required args; got %v", req.Args)
	}
}
