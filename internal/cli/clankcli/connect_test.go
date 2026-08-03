package clankcli

// `clank connect` tests. The catalog reads run against a real host
// service behind a real gateway, reached through the same daemonclient
// production uses — nothing about the auth surface is stubbed.

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supaclank/clank/internal/agent"
	"github.com/supaclank/clank/internal/config"
	daemonclient "github.com/supaclank/clank/internal/daemonclient"
	"github.com/supaclank/clank/internal/host"
	"github.com/supaclank/clank/internal/host/hosttest"
	hostmux "github.com/supaclank/clank/internal/host/mux"
	"github.com/supaclank/clank/pkg/auth"
	"github.com/supaclank/clank/pkg/gateway"
	"github.com/supaclank/clank/pkg/provisioner"
)

// credentialEnvNames is every env var the host reads as a provider
// credential (see internal/host's env-credential map). Tests that need
// "this machine has no agent" clear them all, so a developer's exported
// key can't decide the outcome. A provider added there without a line
// here shows up as a machine-dependent failure in
// TestEnsureAgentConnected_NoCredentials.
var credentialEnvNames = []string{
	"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN",
	"CODEX_API_KEY", "OPENAI_API_KEY",
	"GOOGLE_API_KEY", "GOOGLE_GENERATIVE_AI_API_KEY", "GEMINI_API_KEY",
	"XAI_API_KEY", "GROQ_API_KEY", "DEEPSEEK_API_KEY", "MISTRAL_API_KEY",
	"OPENROUTER_API_KEY",
	"AZURE_RESOURCE_NAME", "AZURE_API_KEY",
	"CLOUDFLARE_ACCOUNT_ID", "CLOUDFLARE_API_KEY",
	"CLOUDFLARE_API_TOKEN", "CLOUDFLARE_GATEWAY_ID",
}

// newConnectTestClient wires daemonclient → gateway → host service, the
// same chain `clank connect` walks in production. HOME is redirected to
// a temp dir first so the host's AuthManager finds no stored
// credentials from the developer's machine.
//
// Not parallelizable: t.Setenv is process-global.
func newConnectTestClient(t *testing.T) *daemonclient.Client {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLANK_DIR", t.TempDir())

	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &hosttest.StubBackendManager{},
		},
	})
	t.Cleanup(svc.Shutdown)

	hostHTTP := httptest.NewServer(hostmux.New(svc, nil).Handler())
	t.Cleanup(hostHTTP.Close)

	gw, err := gateway.NewGateway(gateway.Config{
		Provisioner: &fixedHostProvisioner{url: hostHTTP.URL, transport: http.DefaultTransport},
	}, nil)
	if err != nil {
		t.Fatalf("gateway.NewGateway: %v", err)
	}
	gwSrv := httptest.NewServer(auth.Middleware(gw.Handler(), &auth.AllowAll{UserID: "local"}))
	t.Cleanup(gwSrv.Close)

	return daemonclient.NewTCPClient(gwSrv.URL, "")
}

// A machine with no credentials anywhere must be told so — and must be
// told through a real /hosts/local/auth/providers round trip, which is
// also the routing regression the provider modal hit in the wild (a
// gateway that doesn't strip the /hosts/{name} prefix 404s, which would
// read as agentConnectUnknown here).
func TestEnsureAgentConnected_NoCredentials(t *testing.T) {
	for _, name := range credentialEnvNames {
		t.Setenv(name, "")
	}
	client := newConnectTestClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var out bytes.Buffer
	// Non-TTY in/out: the flow reports rather than opening a UI nobody
	// can drive.
	got := ensureAgentConnected(ctx, client, "", strings.NewReader(""), &out)

	if got == agentConnectUnknown {
		t.Fatalf("catalog read failed — the connect check never reached the host. Output:\n%s", out.String())
	}
	if got != agentNotConnected {
		t.Fatalf("state = %v, want agentNotConnected. Either a credential env var leaked in "+
			"(is one missing from credentialEnvNames?) or $HOME isolation broke. Output:\n%s", got, out.String())
	}
	if !strings.Contains(out.String(), "No coding agent is connected") {
		t.Errorf("user was not told nothing is connected:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "clank connect") {
		t.Errorf("output does not point at the fix:\n%s", out.String())
	}
}

// An env-borne key is a working credential — the spawned agent inherits
// it — so the first-run check must stay quiet rather than nagging a
// user whose machine is already set up.
func TestEnsureAgentConnected_EnvCredentialIsEnough(t *testing.T) {
	client := newConnectTestClient(t)
	t.Setenv(host.EnvAnthropicAPIKey, "sk-ant-test")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var out bytes.Buffer
	if got := ensureAgentConnected(ctx, client, "", strings.NewReader(""), &out); got != agentConnected {
		t.Fatalf("state = %v, want agentConnected. Output:\n%s", got, out.String())
	}
	if out.Len() != 0 {
		t.Errorf("a connected machine must not be prompted:\n%s", out.String())
	}
}

// `clank preview --backend X` must check that X specifically is
// connected, not "is anything connected anywhere" — otherwise a preview
// pinned to an unconnected backend skips the offer just because some
// other backend already has a credential.
func TestEnsureAgentConnected_BackendFlagChecksThatBackendSpecifically(t *testing.T) {
	client := newConnectTestClient(t)
	t.Setenv(host.EnvAnthropicAPIKey, "sk-ant-test") // connects claude-code, not opencode

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var out bytes.Buffer
	got := ensureAgentConnected(ctx, client, agent.BackendOpenCode, strings.NewReader(""), &out)
	if got != agentNotConnected {
		t.Fatalf("state = %v, want agentNotConnected — claude-code being connected must not "+
			"excuse an unconnected opencode. Output:\n%s", got, out.String())
	}
}

// A host that can't be reached is "we don't know", not "you have no
// agent" — guessing would print a fix for a problem the user doesn't
// have while the real failure is about to surface elsewhere.
func TestEnsureAgentConnected_UnreachableHostIsUnknown(t *testing.T) {
	t.Parallel()
	// A gateway URL that refuses connections: the port is closed the
	// instant the listener hands it back.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var out bytes.Buffer
	client := daemonclient.NewTCPClient("http://"+addr, "")
	if got := ensureAgentConnected(ctx, client, "", strings.NewReader(""), &out); got != agentConnectUnknown {
		t.Fatalf("state = %v, want agentConnectUnknown", got)
	}
	if out.Len() != 0 {
		t.Errorf("an unreachable host must not produce advice:\n%s", out.String())
	}
}

// The connect UI needs a terminal. A piped or redirected invocation has
// to say so and return — never hang waiting for keys that can't arrive,
// and never boot a daemon for a UI it can't show.
func TestRunConnect_NonTTYFailsCleanly(t *testing.T) {
	t.Parallel()
	err := runConnect(context.Background(), "", strings.NewReader(""), &bytes.Buffer{})
	if !errors.Is(err, errConnectNeedsTTY) {
		t.Fatalf("err = %v, want errConnectNeedsTTY", err)
	}
	if !strings.Contains(err.Error(), "clank connect") {
		t.Errorf("error does not name the command to run interactively: %v", err)
	}
}

// os.Stdin/os.Stdout under `go test` are not terminals, so the TTY gate
// must reject them too — this is what a CI or piped run really looks
// like.
func TestIsInteractiveTerminal_TestProcessIsNotATTY(t *testing.T) {
	t.Parallel()
	if isInteractiveTerminal(os.Stdin, os.Stdout) {
		t.Skip("test process has a TTY attached; the gate can't be exercised here")
	}
	if isInteractiveTerminal(strings.NewReader(""), &bytes.Buffer{}) {
		t.Error("non-file streams must never read as a terminal")
	}
}

// A first connect should become the default the next session uses —
// otherwise connecting claude-code and then running a session on
// agent.DefaultBackend (opencode, unconnected) fails for no visible
// reason.
func TestAdoptDefaultBackend_FillsAnEmptyPreference(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir())

	var out bytes.Buffer
	if err := adoptDefaultBackend(agent.BackendClaudeCode, &out); err != nil {
		t.Fatalf("adoptDefaultBackend: %v", err)
	}
	prefs, err := config.LoadPreferences()
	if err != nil {
		t.Fatalf("LoadPreferences: %v", err)
	}
	if prefs.DefaultBackend != string(agent.BackendClaudeCode) {
		t.Errorf("DefaultBackend = %q, want %q", prefs.DefaultBackend, agent.BackendClaudeCode)
	}
	if !strings.Contains(out.String(), string(agent.BackendClaudeCode)) {
		t.Errorf("the change was made silently:\n%s", out.String())
	}

	// And it must resolve back through the flag → preference → default
	// chain the session entry points use.
	resolved, err := resolveBackend("", &bytes.Buffer{})
	if err != nil {
		t.Fatalf("resolveBackend: %v", err)
	}
	if resolved != agent.BackendClaudeCode {
		t.Errorf("resolveBackend = %q, want the just-adopted %q", resolved, agent.BackendClaudeCode)
	}
}

// Connecting a second backend must not silently repoint the user's
// existing choice.
func TestAdoptDefaultBackend_NeverOverwritesAChoice(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir())

	if err := config.SavePreferences(config.Preferences{DefaultBackend: string(agent.BackendOpenCode)}); err != nil {
		t.Fatalf("SavePreferences: %v", err)
	}
	var out bytes.Buffer
	if err := adoptDefaultBackend(agent.BackendClaudeCode, &out); err != nil {
		t.Fatalf("adoptDefaultBackend: %v", err)
	}
	prefs, err := config.LoadPreferences()
	if err != nil {
		t.Fatalf("LoadPreferences: %v", err)
	}
	if prefs.DefaultBackend != string(agent.BackendOpenCode) {
		t.Errorf("DefaultBackend = %q, want the user's own %q untouched", prefs.DefaultBackend, agent.BackendOpenCode)
	}
	if out.Len() != 0 {
		t.Errorf("nothing changed, so nothing should have been announced:\n%s", out.String())
	}
}

// Two connects racing to adopt an empty default must not both win: the
// "is it still empty?" check has to run inside UpdatePreferences' own
// lock, or a second caller's stale read clobbers the first's choice and
// both callers report a successful adoption.
func TestAdoptDefaultBackend_ConcurrentCallsAdoptExactlyOnce(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir())

	const n = 50
	backends := []agent.BackendType{agent.BackendClaudeCode, agent.BackendOpenCode}
	outs := make([]bytes.Buffer, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			if err := adoptDefaultBackend(backends[i%len(backends)], &outs[i]); err != nil {
				t.Errorf("adoptDefaultBackend: %v", err)
			}
		}(i)
	}
	wg.Wait()

	adoptions := 0
	for i := range outs {
		if outs[i].Len() > 0 {
			adoptions++
		}
	}
	if adoptions != 1 {
		t.Errorf("got %d concurrent callers announcing adoption, want exactly 1", adoptions)
	}
}

// An unusable --backend must fail before the preview does any work, and
// with the same message `clank connect <bad>` gives — not silently fall
// back to the default agent.
func TestOfferPreviewAgentConnect_RejectsUnknownBackend(t *testing.T) {
	t.Parallel()
	err := offerPreviewAgentConnect(context.Background(), nil, "not-an-agent", strings.NewReader(""), &bytes.Buffer{})
	if err == nil {
		t.Fatal("an unknown --backend must be an error, not a fallback")
	}
	if !strings.Contains(err.Error(), "not-an-agent") {
		t.Errorf("error does not name the bad value: %v", err)
	}
}

// fixedHostProvisioner points every EnsureHost at the in-process host.
type fixedHostProvisioner struct {
	url       string
	transport http.RoundTripper
}

func (f *fixedHostProvisioner) EnsureHost(context.Context, string) (provisioner.HostRef, error) {
	return provisioner.HostRef{URL: f.url, Transport: f.transport, Hostname: host.HostLocal}, nil
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
