package daemoncli

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supaclank/clank/internal/agent"
	"github.com/supaclank/clank/internal/daemonclient"
	"github.com/supaclank/clank/internal/host"
	"github.com/supaclank/clank/internal/host/hosttest"
	hostmux "github.com/supaclank/clank/internal/host/mux"
	hoststore "github.com/supaclank/clank/internal/host/store"
	"github.com/supaclank/clank/pkg/auth"
	"github.com/supaclank/clank/pkg/gateway"
	"github.com/supaclank/clank/pkg/provisioner"
)

// TestLocalE2E_TUICreatesSession_AndFetches drives the full local-mode
// wire end to end: a daemonclient (TUI's HTTP client) talks to a gateway,
// which talks to an in-process host service backed by a stub backend
// manager. POST /sessions must return a SessionInfo with a non-empty ID
// (the host generates the ID; pre-PR-3 the hub did) and GET /sessions/{id}
// must return that same SessionInfo augmented with the live backend's
// runtime status.
//
// This test exists because the PR 3 hub deletion silently broke the
// "session_id is required" contract on POST /sessions — the host's
// create handler still expected a hub-assigned ID after the hub was
// removed. The fix moves ID generation onto the host. This regression
// test pins that contract.
func TestLocalE2E_TUICreatesSession_AndFetches(t *testing.T) {
	t.Parallel()

	// Stub backend so the test never touches opencode/claude.
	stub := &hosttest.StubBackendManager{}

	repo := hosttest.InitGitRepo(t)

	// Real host store at a temp DB so handleGetSession's
	// GetSessionMetadata path is exercised.
	dbPath := filepath.Join(t.TempDir(), "host.db")
	hs, err := hoststore.Open(dbPath)
	if err != nil {
		t.Fatalf("hoststore.Open: %v", err)
	}
	t.Cleanup(func() { hs.Close() })

	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: stub,
		},
		SessionsStore: hs,
	})
	t.Cleanup(svc.Shutdown)

	hostHTTP := httptest.NewServer(hostmux.New(svc, nil).Handler())
	t.Cleanup(hostHTTP.Close)

	// Gateway in front of the host. AllowAll resolves every request to
	// the "local" laptop user — file-perms gate the unix socket in
	// production; tests just inject the principal directly.
	gw, err := gateway.NewGateway(gateway.Config{
		Provisioner: &fixedHostProvisioner{
			url:       hostHTTP.URL,
			transport: http.DefaultTransport,
		},
	}, nil)
	if err != nil {
		t.Fatalf("gateway.NewGateway: %v", err)
	}
	gwSrv := httptest.NewServer(auth.Middleware(gw.Handler(), &auth.AllowAll{UserID: "local"}))
	t.Cleanup(gwSrv.Close)

	// daemonclient is the TUI's HTTP client; same one production wires.
	cli := daemonclient.NewTCPClient(gwSrv.URL, "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	created, err := cli.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{LocalPath: repo, WorktreeID: "01JV7T7F9Y6XQ1R6M8R2W4K3NZ"},
		Prompt:  "hello",
		Config:  workstationConfig(agent.BackendOpenCode),
	})
	if err != nil {
		t.Fatalf("Sessions().Create: %v", err)
	}
	if created.ID == "" {
		t.Fatal("Sessions().Create returned SessionInfo with empty ID")
	}
	if created.Backend != agent.BackendOpenCode {
		t.Errorf("Backend: got %q, want %q", created.Backend, agent.BackendOpenCode)
	}
	if created.Prompt != "hello" {
		t.Errorf("Prompt: got %q, want %q", created.Prompt, "hello")
	}

	// The host must dispatch the initial prompt via OpenAndSend during
	// the create handler. This is the contract the hub used to own
	// (and that PR 3 silently broke until phase 6 — the symptom was
	// "session not started" / spinning busy with no agent reply). We
	// pin it here so the regression cannot return.
	lastBackend := stub.Last()
	if lastBackend == nil {
		t.Fatal("no backend created — session was not started")
	}
	openAndSend := lastBackend.OpenAndSendCalled()
	gotText := lastBackend.LastSendOpts().Text
	if !openAndSend {
		t.Error("handleCreateSession did not call OpenAndSend on the backend")
	}
	if gotText != "hello" {
		t.Errorf("OpenAndSend received text %q, want %q", gotText, "hello")
	}

	// GET /sessions/{id} must return SessionInfo (not the lightweight
	// SessionSnapshot) so the TUI's Session(id).Get() works.
	got, err := cli.Session(created.ID).Get(ctx)
	if err != nil {
		t.Fatalf("Session(%s).Get: %v", created.ID, err)
	}
	if got.ID != created.ID {
		t.Errorf("ID round-trip: got %q, want %q", got.ID, created.ID)
	}
	if got.Backend != agent.BackendOpenCode {
		t.Errorf("Backend round-trip: got %q, want %q", got.Backend, agent.BackendOpenCode)
	}

	// Host-scoped routes go through /hosts/{hostname}/... in the TUI's
	// HostClient. The gateway must strip that prefix before forwarding,
	// otherwise the host returns 404 "page not found" — that was the
	// symptom for the "connect provider" modal failing in the wild.
	// AuthManager is now created unconditionally (not only when the
	// opencode backend exists, since Anthropic providers need it too),
	// so a successful call with a populated catalog is the routing-
	// works signal. A 404 here would mean the prefix wasn't stripped.
	providers, authErr := cli.Host("local").ListAuthProviders(ctx, "")
	if authErr != nil {
		if strings.Contains(authErr.Error(), "404 page not found") {
			t.Errorf("gateway did not strip /hosts/{name} prefix; got 404: %v", authErr)
		} else {
			t.Errorf("ListAuthProviders: %v", authErr)
		}
	}
	if len(providers) == 0 {
		t.Error("expected non-empty provider catalog (routing reached the host)")
	}
}

// fixedHostProvisioner returns the same HostRef on every EnsureHost. The
// test wires it to the in-process host's HTTP test server.
type fixedHostProvisioner struct {
	url       string
	transport http.RoundTripper
}

func (f *fixedHostProvisioner) EnsureHost(context.Context, string) (provisioner.HostRef, error) {
	return provisioner.HostRef{URL: f.url, Transport: f.transport, Hostname: "local"}, nil
}
func (*fixedHostProvisioner) SuspendHost(context.Context, string) error        { return nil }
func (*fixedHostProvisioner) DestroyHost(context.Context, string) error        { return nil }
func (*fixedHostProvisioner) DestroyHostsByUser(context.Context, string) error { return nil }
func (*fixedHostProvisioner) GetHostByID(context.Context, string) (provisioner.HostRef, error) {
	return provisioner.HostRef{}, errors.New("fixedHostProvisioner: GetHostByID not implemented")
}
func (*fixedHostProvisioner) OpenInternalConn(context.Context, string, int) (net.Conn, error) {
	return nil, errors.New("fixedHostProvisioner: OpenInternalConn not implemented")
}
