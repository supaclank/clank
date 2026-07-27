package flymachines

import (
	"fmt"
	"maps"

	fly "github.com/superfly/fly-go"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/pkg/provisioner"
)

// buildMachineConfig assembles the desired config for a user's
// machine. Pure — the unit-test surface for everything the machine's
// behavior hangs on.
//
// Load-bearing choices:
//   - Restart policy "no" + fly-proxy Autostop OFF: sleep is OWNED by
//     clank-host's exit keepalive (idle self-exit → machine stops).
//     Proxy autostop would kill long agent runs that have no inbound
//     traffic; a restart policy would resurrect a deliberate idle exit.
//   - Autostart ON: the Flycast dial wakes a stopped machine, which is
//     the entire wake path (HostRef.AutoWake=true).
//   - One raw-TCP service on HostPort, no handlers: gateway↔machine
//     traffic is in-org (Flycast); TLS terminates at the gateway edge.
func buildMachineConfig(opts Options, tokens hostTokens, volumeID string, oneShotEnv map[string]string) *fly.MachineConfig {
	env := map[string]string{
		"CLANK_HOST_PORT":          fmt.Sprintf("%d", HostPort),
		"CLANK_HOST_AUTH_TOKEN":    tokens.auth,
		"CLANK_NOTIFIER_TOKEN":     tokens.notifier,
		"CLANK_KEEPALIVE_PROVIDER": "exit",
		// Machines are disposable: new sessions run without permission
		// prompts unless the client picks a mode itself. Hosts that are
		// not provisioned by us (a laptop, a self-hosted box) omit this
		// and get clank-host's conservative default.
		"CLANK_PERMISSION_POSTURE": string(agent.PosturePermissive),
	}
	if opts.NotifierWebhookURL != "" {
		env["CLANK_NOTIFIER_PROVIDER"] = "webhook"
		env["CLANK_NOTIFIER_WEBHOOK_URL"] = opts.NotifierWebhookURL
	}
	if opts.PreviewWebhookURL != "" {
		env["CLANK_PREVIEW_WEBHOOK_URL"] = opts.PreviewWebhookURL
	}
	if opts.GitHubOAuthClientID != "" {
		env["CLANK_GITHUB_OAUTH_CLIENT_ID"] = opts.GitHubOAuthClientID
	}
	if tj := provisioner.TemplatesEnvValue(opts.Templates); tj != "" {
		// Builtin create-project catalog. Part of the machine config
		// (not one-shot env) so it's steady-state and survives the drift
		// reconcile — else a machine host serves an empty GET /templates.
		env["CLANK_TEMPLATES"] = tj
	}
	maps.Copy(env, oneShotEnv)

	swap := opts.SwapSizeMB
	return &fly.MachineConfig{
		Image: opts.Image,
		Env:   env,
		Guest: &fly.MachineGuest{
			CPUKind:  opts.GuestCPUKind,
			CPUs:     opts.GuestCPUs,
			MemoryMB: opts.GuestMemoryMB,
		},
		Init:    fly.MachineInit{SwapSizeMB: &swap},
		Restart: &fly.MachineRestart{Policy: fly.MachineRestartPolicyNo},
		Mounts: []fly.MachineMount{{
			Volume:                 volumeID,
			Path:                   "/data",
			ExtendThresholdPercent: 80,
			AddSizeGb:              DefaultVolumeExtendGB,
			// Auto-extend ceiling must be ≥ the volume's own size, else
			// Fly rejects the config / auto-extension is inert. withDefaults
			// guarantees VolumeSizeGB ≤ DefaultVolumeSizeLimitGB, so take
			// whichever is larger to stay correct if that ever changes.
			SizeGbLimit: max(DefaultVolumeSizeLimitGB, opts.VolumeSizeGB),
		}},
		Services: []fly.MachineService{{
			Protocol:     "tcp",
			InternalPort: HostPort,
			Ports:        []fly.MachinePort{{Port: new(int(HostPort))}},
			Autostart:    new(true),
			Autostop:     new(fly.MachineAutostopOff),
		}},
	}
}

// guestPreset renders the canonical preset string usage metering keys
// on, e.g. "shared-8x-4096".
func guestPreset(opts Options) string {
	return fmt.Sprintf("%s-%dx-%d", opts.GuestCPUKind, opts.GuestCPUs, opts.GuestMemoryMB)
}

// needsUpdate reports whether the live machine config drifted from
// want on any field this provisioner owns. Compares ONLY owned fields
// — the API fills defaults (init, metadata, …) that must not read as
// drift. An update restarts the workload, so false positives are the
// dangerous direction; the matrix test pins both directions.
func needsUpdate(have, want *fly.MachineConfig) bool {
	if have == nil {
		return true
	}
	if have.Image != want.Image {
		return true
	}
	if !maps.Equal(have.Env, want.Env) {
		return true
	}
	if have.Guest == nil ||
		have.Guest.CPUKind != want.Guest.CPUKind ||
		have.Guest.CPUs != want.Guest.CPUs ||
		have.Guest.MemoryMB != want.Guest.MemoryMB {
		return true
	}
	if intPtrVal(have.Init.SwapSizeMB) != intPtrVal(want.Init.SwapSizeMB) {
		return true
	}
	if have.Restart == nil || have.Restart.Policy != want.Restart.Policy {
		return true
	}
	if len(have.Mounts) != 1 || mountDiffers(have.Mounts[0], want.Mounts[0]) {
		return true
	}
	if len(have.Services) != 1 || serviceDiffers(have.Services[0], want.Services[0]) {
		return true
	}
	return false
}

func mountDiffers(have, want fly.MachineMount) bool {
	return have.Volume != want.Volume ||
		have.Path != want.Path ||
		have.ExtendThresholdPercent != want.ExtendThresholdPercent ||
		have.AddSizeGb != want.AddSizeGb ||
		have.SizeGbLimit != want.SizeGbLimit
}

func serviceDiffers(have, want fly.MachineService) bool {
	if have.Protocol != want.Protocol || have.InternalPort != want.InternalPort {
		return true
	}
	if len(have.Ports) != 1 || len(want.Ports) != 1 ||
		intPtrVal(have.Ports[0].Port) != intPtrVal(want.Ports[0].Port) ||
		len(have.Ports[0].Handlers) != len(want.Ports[0].Handlers) {
		return true
	}
	if boolPtrVal(have.Autostart) != boolPtrVal(want.Autostart) {
		return true
	}
	// The API answers old-format booleans for off/stop; the typed
	// MachineAutostop unmarshals both, so direct compare is safe.
	if autostopVal(have.Autostop) != autostopVal(want.Autostop) {
		return true
	}
	return false
}

func intPtrVal(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func boolPtrVal(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}

func autostopVal(p *fly.MachineAutostop) fly.MachineAutostop {
	if p == nil {
		return fly.MachineAutostopOff
	}
	return *p
}
