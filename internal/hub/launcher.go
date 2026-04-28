package hub

import (
	"context"
	"fmt"
	"sync"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
	hostclient "github.com/acksell/clank/internal/host/client"
)

// HostLauncher provisions a fresh Host plane on demand and returns a
// client to it. The cloud-hub flow:
//
//  1. Mobile/CLI POSTs /sessions with LaunchHost set.
//  2. createSession looks up the launcher for spec.Provider.
//  3. The launcher spawns the host (Daytona sandbox, local clank-host, etc.).
//  4. The hub registers the resulting client in the catalog under the
//     launcher-chosen Hostname and rewrites the request's Hostname so
//     the rest of the create flow proceeds unchanged.
//
// Lifecycle is the launcher's concern. Launchers that own long-lived
// child processes should expose Stop() and be wired into daemoncli's
// shutdown path (or into hub.Service.Stop via a launcher.Closer hook).
type HostLauncher interface {
	Launch(ctx context.Context, spec agent.LaunchHostSpec) (host.Hostname, *hostclient.HTTP, error)
}

// launcherRegistry holds per-provider launchers. Constructed lazily on
// first SetHostLauncher.
type launcherRegistry struct {
	mu       sync.RWMutex
	bindings map[string]HostLauncher
}

// SetHostLauncher registers a HostLauncher under provider name. The
// provider name must match the LaunchHostSpec.Provider that clients
// will send. Re-registering replaces the prior entry; passing nil
// removes it.
func (s *Service) SetHostLauncher(provider string, l HostLauncher) {
	s.launchersMu.Lock()
	defer s.launchersMu.Unlock()
	if s.launchers == nil {
		s.launchers = map[string]HostLauncher{}
	}
	if l == nil {
		delete(s.launchers, provider)
		return
	}
	s.launchers[provider] = l
}

// hostLauncher returns the launcher registered for provider, or an
// error if none.
func (s *Service) hostLauncher(provider string) (HostLauncher, error) {
	s.launchersMu.RLock()
	defer s.launchersMu.RUnlock()
	l, ok := s.launchers[provider]
	if !ok {
		return nil, fmt.Errorf("no host launcher registered for provider %q", provider)
	}
	return l, nil
}

// SetDefaultLaunchHost configures a fallback LaunchHostSpec applied to
// any StartRequest whose LaunchHost is nil. Used on cloud hubs so
// TUI-created sessions (which don't pick a launcher) automatically
// spawn a sandbox. Pass nil to clear.
//
// Wired by daemoncli from preferences.default_launch_host_provider so
// users can opt in by editing one config field.
func (s *Service) SetDefaultLaunchHost(spec *agent.LaunchHostSpec) {
	s.launchersMu.Lock()
	defer s.launchersMu.Unlock()
	if spec == nil {
		s.defaultLaunchHostSpec = nil
		return
	}
	// Defensive copy so callers mutating the spec post-set don't race.
	cp := *spec
	s.defaultLaunchHostSpec = &cp
}
