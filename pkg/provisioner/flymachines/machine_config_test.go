package flymachines

import (
	"testing"

	fly "github.com/superfly/fly-go"

	"github.com/acksell/clank/internal/agent/presets"
)

func testOptions(t *testing.T) Options {
	t.Helper()
	opts, err := Options{
		APIToken: "tok",
		OrgSlug:  "org",
		Region:   "arn",
		Image:    "ghcr.io/example/clank-host-fly@sha256:aaaa",
	}.withDefaults()
	if err != nil {
		t.Fatalf("withDefaults: %v", err)
	}
	return opts
}

func testTokens() hostTokens {
	return hostTokens{auth: "auth-token", notifier: "clnk_notifier"}
}

// TestMachineConfigSleepWakeContract pins the three settings the whole
// wake model hangs on. Regression guards:
//   - Autostop must stay OFF: fly-proxy autostop watches inbound
//     connections, and a long agent run has none — proxy autostop
//     would kill mid-run agents. Idle exit is clank-host's job.
//   - Restart policy must stay "no": anything else resurrects a
//     deliberate idle exit and the machine never sleeps.
//   - Autostart must stay ON: the Flycast dial is the only wake path.
func TestMachineConfigSleepWakeContract(t *testing.T) {
	t.Parallel()
	cfg := buildMachineConfig(testOptions(t), testTokens(), "vol_1", nil)

	if n := len(cfg.Services); n != 1 {
		t.Fatalf("want exactly 1 service, got %d", n)
	}
	svc := cfg.Services[0]
	if got := autostopVal(svc.Autostop); got != fly.MachineAutostopOff {
		t.Errorf("Autostop = %v, want off — proxy autostop kills long agent runs with no inbound traffic", got)
	}
	if !boolPtrVal(svc.Autostart) {
		t.Error("Autostart = false, want true — the Flycast dial is the only wake path")
	}
	if cfg.Restart == nil || cfg.Restart.Policy != fly.MachineRestartPolicyNo {
		t.Errorf("Restart = %+v, want policy \"no\" — anything else resurrects a deliberate idle exit", cfg.Restart)
	}
	if svc.Protocol != "tcp" || svc.InternalPort != HostPort {
		t.Errorf("service = %s/%d, want tcp/%d", svc.Protocol, svc.InternalPort, HostPort)
	}
	if len(svc.Ports) != 1 || len(svc.Ports[0].Handlers) != 0 {
		t.Errorf("ports = %+v, want one raw-TCP port with no handlers (TLS terminates at the gateway)", svc.Ports)
	}
}

func TestMachineConfigShape(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	opts.NotifierWebhookURL = "https://gw.example.com/webhooks/notifications"
	opts.PreviewWebhookURL = "https://gw.example.com/webhooks/preview"
	opts.GitHubOAuthClientID = "ghclient"
	cfg := buildMachineConfig(opts, testTokens(), "vol_1", map[string]string{"CLANK_RESTORE_URL": "https://s3/x.tgz"})

	wantEnv := map[string]string{
		"CLANK_HOST_PORT":              "8080",
		"CLANK_HOST_AUTH_TOKEN":        "auth-token",
		"CLANK_NOTIFIER_TOKEN":         "clnk_notifier",
		"CLANK_KEEPALIVE_PROVIDER":     "exit",
		"CLANK_NOTIFIER_PROVIDER":      "webhook",
		"CLANK_NOTIFIER_WEBHOOK_URL":   "https://gw.example.com/webhooks/notifications",
		"CLANK_PREVIEW_WEBHOOK_URL":    "https://gw.example.com/webhooks/preview",
		"CLANK_GITHUB_OAUTH_CLIENT_ID": "ghclient",
		"CLANK_RESTORE_URL":            "https://s3/x.tgz",
		"CLANK_BUILTIN_PRESETS":        presets.EnvValue(presets.Sandbox()),
	}
	for k, want := range wantEnv {
		if got := cfg.Env[k]; got != want {
			t.Errorf("env[%s] = %q, want %q", k, got, want)
		}
	}
	if len(cfg.Env) != len(wantEnv) {
		t.Errorf("env has %d entries, want %d: %v", len(cfg.Env), len(wantEnv), cfg.Env)
	}

	if cfg.Guest.CPUKind != "shared" || cfg.Guest.CPUs != 8 || cfg.Guest.MemoryMB != 4096 {
		t.Errorf("guest = %+v, want shared/8/4096 defaults", cfg.Guest)
	}
	if got := intPtrVal(cfg.Init.SwapSizeMB); got != DefaultSwapSizeMB {
		t.Errorf("swap = %d, want %d", got, DefaultSwapSizeMB)
	}
	if len(cfg.Mounts) != 1 {
		t.Fatalf("want 1 mount, got %d", len(cfg.Mounts))
	}
	m := cfg.Mounts[0]
	if m.Volume != "vol_1" || m.Path != "/data" {
		t.Errorf("mount = %+v, want vol_1 at /data", m)
	}
	if m.ExtendThresholdPercent != 80 || m.AddSizeGb != DefaultVolumeExtendGB || m.SizeGbLimit != DefaultVolumeSizeLimitGB {
		t.Errorf("mount auto-extend = %+v, want 80%%/+%dGB/limit %dGB", m, DefaultVolumeExtendGB, DefaultVolumeSizeLimitGB)
	}

	if got := guestPreset(opts); got != "shared-8x-4096" {
		t.Errorf("guestPreset = %q, want shared-8x-4096", got)
	}
}

// TestNeedsUpdateMatrix pins drift detection in both directions. A
// machine update RESTARTS the workload, so a false positive is the
// dangerous direction — identical configs must never read as drift.
func TestNeedsUpdateMatrix(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	tokens := testTokens()
	base := func() *fly.MachineConfig { return buildMachineConfig(opts, tokens, "vol_1", nil) }

	cases := []struct {
		name   string
		mutate func(*fly.MachineConfig)
		want   bool
	}{
		{"identical", func(*fly.MachineConfig) {}, false},
		{"nil-have", nil, true},
		{"image-bump", func(c *fly.MachineConfig) { c.Image = "ghcr.io/example/clank-host-fly@sha256:bbbb" }, true},
		{"env-token-rotation", func(c *fly.MachineConfig) { c.Env["CLANK_HOST_AUTH_TOKEN"] = "rotated" }, true},
		{"env-extra-key", func(c *fly.MachineConfig) { c.Env["NEW"] = "x" }, true},
		{"guest-ram-bump", func(c *fly.MachineConfig) { c.Guest.MemoryMB = 8192 }, true},
		{"swap-change", func(c *fly.MachineConfig) { v := 4096; c.Init.SwapSizeMB = &v }, true},
		{"restart-policy", func(c *fly.MachineConfig) { c.Restart.Policy = fly.MachineRestartPolicyAlways }, true},
		{"volume-swap", func(c *fly.MachineConfig) { c.Mounts[0].Volume = "vol_2" }, true},
		{"autoextend-limit", func(c *fly.MachineConfig) { c.Mounts[0].SizeGbLimit = 100 }, true},
		{"autostop-flip", func(c *fly.MachineConfig) { c.Services[0].Autostop = new(fly.MachineAutostopStop) }, true},
		{"autostart-flip", func(c *fly.MachineConfig) { c.Services[0].Autostart = new(false) }, true},
		// The API answers old configs with fields WE don't own filled
		// in — those must not read as drift.
		{"api-filled-metadata", func(c *fly.MachineConfig) { c.Metadata = map[string]string{"fly_platform_version": "v2"} }, false},
		{"api-filled-max-memory", func(c *fly.MachineConfig) { c.Guest.MaxMemoryMB = 8192 }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			want := base()
			var have *fly.MachineConfig
			if c.mutate != nil {
				have = base()
				c.mutate(have)
			}
			if got := needsUpdate(have, want); got != c.want {
				t.Errorf("needsUpdate = %v, want %v", got, c.want)
			}
		})
	}
}

// TestOneShotEnvCarriedForward: a machine cold-created with
// CLANK_RESTORE_URL must not read as drifted forever after.
func TestOneShotEnvCarriedForward(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	tokens := testTokens()

	live := buildMachineConfig(opts, tokens, "vol_1", map[string]string{"CLANK_RESTORE_URL": "https://s3/x.tgz"})
	want := buildMachineConfig(opts, tokens, "vol_1", oneShotEnv(live))
	if needsUpdate(live, want) {
		t.Error("restore-url machine reads as perpetual drift — oneShotEnv not carried forward")
	}

	// And a machine without it must not inherit one.
	plain := buildMachineConfig(opts, tokens, "vol_1", nil)
	if oneShotEnv(plain) != nil {
		t.Error("oneShotEnv invented a restore URL for a plain config")
	}
}
