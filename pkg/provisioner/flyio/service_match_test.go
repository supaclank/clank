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
	want := buildServiceRequest(hostTokens{auth: "tok-abc"}, "")
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
	want := buildServiceRequest(hostTokens{auth: "tok"}, "")
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
	want := buildServiceRequest(hostTokens{auth: "new-token"}, "")
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
	want := buildServiceRequest(hostTokens{auth: "tok"}, "")
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
	want := buildServiceRequest(hostTokens{auth: "tok"}, "")
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
	req := buildServiceRequest(hostTokens{auth: "tok"}, "")
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
	want := buildServiceRequest(hostTokens{auth: "tok"}, "")
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

// TestBuildServiceRequest_OmitsNotifierFlagsWhenURLEmpty pins the
// laptop-dev / no-dispatcher path: without a webhook URL configured,
// clank-host shouldn't get the notifier flags at all (the default
// --notifier-provider=none stays in effect).
func TestBuildServiceRequest_OmitsNotifierFlagsWhenURLEmpty(t *testing.T) {
	t.Parallel()
	req := buildServiceRequest(hostTokens{auth: "tok", notifier: "clnk_x"}, "")
	for _, a := range req.Args {
		if a == "--notifier-provider" || a == "--notifier-webhook-url" || a == "--notifier-webhook-token" {
			t.Errorf("notifier flag %q present when webhook URL is empty; args=%v", a, req.Args)
		}
	}
}

// TestBuildServiceRequest_EmitsNotifierFlagsWhenConfigured pins the
// production path: a configured webhook URL + minted notifier token
// produces the three --notifier-* flags clank-host expects.
func TestBuildServiceRequest_EmitsNotifierFlagsWhenConfigured(t *testing.T) {
	t.Parallel()
	req := buildServiceRequest(hostTokens{auth: "tok", notifier: "clnk_abc"}, "https://disp.example/webhooks/notifications")
	wantPairs := map[string]string{
		"--notifier-provider":      "webhook",
		"--notifier-webhook-url":   "https://disp.example/webhooks/notifications",
		"--notifier-webhook-token": "clnk_abc",
	}
	for flag, want := range wantPairs {
		got := ""
		for i := 0; i < len(req.Args)-1; i++ {
			if req.Args[i] == flag {
				got = req.Args[i+1]
			}
		}
		if got != want {
			t.Errorf("%s = %q, want %q (args=%v)", flag, got, want, req.Args)
		}
	}
}

// TestServiceMatches_NotifierTokenIsWildcarded pins that a notifier
// token rotation alone doesn't force a service recreate, mirroring
// the auth-token wildcard. Recreate is expensive — we only do it on
// genuine flag drift.
func TestServiceMatches_NotifierTokenIsWildcarded(t *testing.T) {
	t.Parallel()
	want := buildServiceRequest(hostTokens{auth: "tok", notifier: "clnk_new"}, "https://disp/x")
	haveArgs := append([]string(nil), want.Args...)
	for i := 0; i < len(haveArgs)-1; i++ {
		if haveArgs[i] == "--notifier-webhook-token" {
			haveArgs[i+1] = "clnk_old"
		}
	}
	have := &sprites.Service{
		Cmd:      want.Cmd,
		Args:     haveArgs,
		HTTPPort: intPtr(*want.HTTPPort),
	}
	if !serviceMatches(have, want) {
		t.Errorf("notifier-token-only delta should be tolerated; have=%v want=%v", have.Args, want.Args)
	}
}

// TestServiceMatches_WebhookURLChangeForcesRecreate pins the dispatch
// destination as a structural arg — if the operator points clank-host
// at a different clankd URL (gateway rename, dev→prod cutover), the
// new arg must trigger a recreate so the host actually picks it up.
func TestServiceMatches_WebhookURLChangeForcesRecreate(t *testing.T) {
	t.Parallel()
	want := buildServiceRequest(hostTokens{auth: "tok", notifier: "clnk_x"}, "https://new-disp/x")
	have := &sprites.Service{
		Cmd:      want.Cmd,
		HTTPPort: intPtr(*want.HTTPPort),
		Args:     []string{},
	}
	// Mirror want.Args except for the webhook URL.
	for i := 0; i < len(want.Args); i++ {
		v := want.Args[i]
		if i > 0 && want.Args[i-1] == "--notifier-webhook-url" {
			v = "https://old-disp/x"
		}
		have.Args = append(have.Args, v)
	}
	if serviceMatches(have, want) {
		t.Error("webhook-URL change should trigger recreate")
	}
}

func TestBuildServiceRequest_OmitsRemovedGitSyncFlags(t *testing.T) {
	t.Parallel()
	req := buildServiceRequest(hostTokens{auth: "tok"}, "")
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
