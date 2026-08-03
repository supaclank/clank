package tui

import (
	"testing"

	"github.com/supaclank/clank/internal/cloud"
	"github.com/supaclank/clank/internal/config"
)

// TestCloudView_LoginResult_FlipsToErrorOnPersistFailure pins the
// contract: if persistSession fails (e.g. disk full, read-only HOME),
// the panel must NOT flip to SignedIn — restart would lose the
// session and the UI card would lie about the on-disk state.
//
// Not t.Parallel: CLANK_DIR is process-global.
func TestCloudView_LoginResult_FlipsToErrorOnPersistFailure(t *testing.T) {
	t.Setenv("CLANK_DIR", "/dev/null/cannot-mkdir")

	m := cloudView{}
	out, _ := m.Update(cloudLoginResultMsg{
		session: &cloud.Session{AccessToken: "tok"},
	})
	if out.phase != cloudPhaseError {
		t.Fatalf("phase = %v, want cloudPhaseError", out.phase)
	}
	if out.err == "" {
		t.Fatal("err is empty; want a 'save session' message")
	}
	if out.session != nil {
		t.Fatalf("session = %v, want nil on persist failure", out.session)
	}
}

// TestCloudView_Reachability_ClearsRevokedToken pins the contract:
// when the gateway returns 401 (revoked credential / mismatched auth
// config), the saved session must be wiped from disk AND from
// memory, so Init() on the next start doesn't reload a dead token
// and render a stale signed-in card.
//
// Not t.Parallel: CLANK_DIR is process-global.
func TestCloudView_Reachability_ClearsRevokedToken(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir())
	if err := config.UpdatePreferences(func(p *config.Preferences) {
		p.Remote = &config.RemoteConfig{
			Active: "default",
			Profiles: map[string]*config.Remote{"default": {
				GatewayURL:   "https://example.test",
				AccessToken:  "tok-revoked",
				RefreshToken: "rt",
				UserEmail:    "u@example.test",
			}},
		}
	}); err != nil {
		t.Fatalf("UpdatePreferences: %v", err)
	}

	m := cloudView{
		phase:   cloudPhaseSignedIn,
		session: &cloud.Session{AccessToken: "tok-revoked"},
	}
	out, _ := m.Update(cloudReachabilityMsg{err: cloud.ErrUnauthorized})

	if out.phase != cloudPhaseSignedOut {
		t.Errorf("phase = %v, want cloudPhaseSignedOut", out.phase)
	}
	if out.session != nil {
		t.Errorf("session = %v, want nil", out.session)
	}
	prefs, err := config.LoadPreferences()
	if err != nil {
		t.Fatalf("LoadPreferences: %v", err)
	}
	active := prefs.ActiveRemote()
	if active == nil {
		t.Fatal("active remote disappeared")
	}
	if active.AccessToken != "" || active.RefreshToken != "" {
		t.Errorf("token not cleared: %+v", active)
	}
}

// TestCloudView_Reachability_KeepsTransportError pins the other half:
// a non-401 error (e.g. connection refused) records the error but
// does NOT touch disk — the token is still valid, the network is
// just down.
func TestCloudView_Reachability_KeepsTransportError(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir())
	if err := config.UpdatePreferences(func(p *config.Preferences) {
		p.Remote = &config.RemoteConfig{
			Active: "default",
			Profiles: map[string]*config.Remote{"default": {
				GatewayURL:  "https://example.test",
				AccessToken: "tok-good",
			}},
		}
	}); err != nil {
		t.Fatalf("UpdatePreferences: %v", err)
	}

	m := cloudView{
		phase:   cloudPhaseSignedIn,
		session: &cloud.Session{AccessToken: "tok-good"},
	}
	out, _ := m.Update(cloudReachabilityMsg{err: errSimulatedTransport})

	if out.phase != cloudPhaseSignedIn {
		t.Errorf("phase = %v, want cloudPhaseSignedIn (transport blip ≠ revoked)", out.phase)
	}
	if out.session == nil || out.session.AccessToken != "tok-good" {
		t.Errorf("session was nilled or mutated: %+v", out.session)
	}
	prefs, _ := config.LoadPreferences()
	if active := prefs.ActiveRemote(); active == nil || active.AccessToken != "tok-good" {
		t.Errorf("disk token cleared on transport blip: %+v", active)
	}
}

// errSimulatedTransport stands in for a connection-refused / DNS /
// timeout error — anything that's not cloud.ErrUnauthorized.
var errSimulatedTransport = simulatedTransportErr{}

type simulatedTransportErr struct{}

func (simulatedTransportErr) Error() string { return "simulated transport error" }
