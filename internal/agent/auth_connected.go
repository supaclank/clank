package agent

// Connection-state reads over a GET /auth/providers snapshot. Callers
// that need "can this machine run an agent at all?" (the CLI's first-run
// gate) and "which backends are ready?" (the connect picker) share these
// so the answer can't drift between entry points.

// IsAnyProviderConnected reports whether at least one provider in the
// snapshot has a usable credential — stored by clank or borrowed from
// the machine's own CLI login / environment.
func IsAnyProviderConnected(providers []ProviderAuthInfo) bool {
	for _, p := range providers {
		if p.Connected {
			return true
		}
	}
	return false
}

// IsBackendConnected reports whether backend has at least one connected
// provider in the snapshot. A snapshot filtered to another backend
// always answers false — it carries no evidence either way.
func IsBackendConnected(providers []ProviderAuthInfo, backend BackendType) bool {
	for _, p := range providers {
		if p.Backend == backend && p.Connected {
			return true
		}
	}
	return false
}
