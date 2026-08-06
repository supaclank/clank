package host

// GH_TOKEN for agents running in this host's sandbox. `gh` resolves
// credentials from its own sources (GH_TOKEN, hosts.yml, keychain) and
// never consults git's credential.helper, so the helper that makes
// `git push` work leaves `gh` logged out on a machine with no gh login.
// The token has to arrive as env.
//
// Per-method file, matching CLAUDE.md's convention for Service methods.

// githubAgentEnv returns GH_TOKEN when clank holds its OWN GitHub
// credential, nil otherwise. Resolved per adapter spawn and per
// reconcile, so a connect or disconnect lands on the next tick without
// any credential callback to wire.
//
// Reads the store directly instead of calling Manager.AccessToken, on
// purpose: AccessToken falls back to `gh auth token` when the host ran
// with --gh-cli-auth, which is the laptop provisioner's mode. Handing a
// borrowed token back to gh as GH_TOKEN would override the very keychain
// entry it came from and pin a snapshot of a credential gh rotates on
// its own, breaking gh later in a way that looks like nothing. Laptops
// therefore resolve to nil here and keep using their own login; remote
// machines, where the fallback is off and the store is the only source,
// get the token.
//
// Stability matters as much as correctness: this feeds the supervisor's
// env fingerprint, so a value that flaps restarts adapters on a timer
// and kills in-flight turns. Store writes are tmp + rename (see
// Store.persistLocked), so a concurrent connect is observed as either
// the old token or the new one, never as a failed read.
func (s *Service) githubAgentEnv() map[string]string {
	if s.github == nil {
		return nil
	}
	c, err := s.github.Store().Read()
	if err != nil || c.AccessToken == "" {
		return nil
	}
	return map[string]string{"GH_TOKEN": c.AccessToken}
}
