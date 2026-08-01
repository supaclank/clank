package clankcli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host/hosttest"
	"github.com/acksell/clank/internal/launchconfig"
)

func TestChoosePreviewSetupScope(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  launchconfig.Scope
	}{
		{name: "empty defaults private", input: "\n", want: launchconfig.ScopeHost},
		{name: "eof defaults private", input: "", want: launchconfig.ScopeHost},
		{name: "no is private", input: "no\n", want: launchconfig.ScopeHost},
		{name: "yes is shared", input: "yes\n", want: launchconfig.ScopeProject},
		{name: "case insensitive", input: "Y\n", want: launchconfig.ScopeProject},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			got, err := choosePreviewSetupScope(strings.NewReader(tt.input), &out)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("scope = %q, want %q", got, tt.want)
			}
			for _, required := range []string{"One-time setup", ".clank/launch.yaml", "[y/N]", "private to this machine"} {
				if !strings.Contains(out.String(), required) {
					t.Errorf("prompt missing %q: %s", required, out.String())
				}
			}
		})
	}
}

func TestChoosePreviewSetupScopeRepromptsInvalidAnswer(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	scope, err := choosePreviewSetupScope(strings.NewReader("maybe\ny\n"), &out)
	if err != nil {
		t.Fatal(err)
	}
	if scope != launchconfig.ScopeProject {
		t.Fatalf("scope = %q", scope)
	}
	if !strings.Contains(out.String(), "Please answer y or n") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestCreatePreviewSetupSessionUsesSelectedPrompt(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir())
	client, stub := newTestHost(t)
	repo := hosttest.InitGitRepo(t)
	paths, err := launchconfig.ResolvePaths(repo)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, events, cancelEvents, err := createPreviewSetupSession(ctx, client, agent.BackendOpenCode, paths, launchconfig.ScopeHost)
	if err != nil {
		t.Fatal(err)
	}
	cancelEvents()
	if events == nil || info.ID == "" {
		t.Fatalf("session = %+v, events nil = %t", info, events == nil)
	}
	last := stub.Last()
	if last == nil || !last.OpenAndSendCalled() {
		t.Fatal("setup prompt was not dispatched")
	}
	prompt := last.LastSendOpts().Text
	staging := filepath.Join(repo, filepath.FromSlash(launchconfig.SetupRelativePath))
	if !strings.Contains(prompt, staging) || !strings.Contains(prompt, "non-interactive") {
		t.Fatalf("setup prompt = %q", prompt)
	}
	if strings.Contains(prompt, "ask whether") {
		t.Fatalf("setup prompt asks agent to select storage: %q", prompt)
	}
}

func TestRunPreviewSetupCompletesInlineAndInstallsPrivateConfig(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir())
	client, stub := newTestHost(t)
	repo := hosttest.InitGitRepo(t)
	paths, err := launchconfig.ResolvePaths(repo)
	if err != nil {
		t.Fatal(err)
	}
	staging, err := launchconfig.SetupOutputPath(paths, launchconfig.ScopeHost)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(staging), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staging, []byte(validPreviewSetupYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out bytes.Buffer
	resolved, err := runPreviewSetup(ctx, client, agent.BackendOpenCode, repo, strings.NewReader("\n"), &out)
	if err != nil {
		t.Fatalf("runPreviewSetup: %v\noutput:\n%s", err, out.String())
	}
	if resolved.Launch.Source.Scope != launchconfig.ScopeHost || resolved.Launch.Source.Path != paths.Host {
		t.Fatalf("resolved source = %+v", resolved.Launch.Source)
	}
	if !strings.Contains(out.String(), "Preview configuration generated") {
		t.Fatalf("output lacks generated configuration: %q", out.String())
	}
	info, err := client.Session(resolved.SessionID).Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Visibility == agent.VisibilityDone {
		t.Fatal("setup session was marked done before the generated server reached readiness")
	}
	completePreviewSetupSession(client, resolved.SessionID, &out)
	info, err = client.Session(resolved.SessionID).Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Visibility != agent.VisibilityDone {
		t.Fatalf("setup session visibility = %q, want done", info.Visibility)
	}
	if last := stub.Last(); last == nil || !last.OpenAndSendCalled() {
		t.Fatal("setup agent did not receive its task")
	}
}

func TestFinalizePreviewSetupRetriesOneInvalidGeneration(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir())
	repo := hosttest.InitGitRepo(t)
	paths, err := launchconfig.ResolvePaths(repo)
	if err != nil {
		t.Fatal(err)
	}
	staging, err := launchconfig.SetupOutputPath(paths, launchconfig.ScopeHost)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(staging), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staging, []byte("prevews: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	correction, resolved, err := inspectPreviewSetup(repo, launchconfig.ScopeHost, &out)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != nil || correction == "" {
		t.Fatalf("correction = %q, resolved = %+v", correction, resolved)
	}
	for _, required := range []string{"validation failed", "field prevews not found", staging, "non-interactive", "one minute"} {
		if !strings.Contains(correction, required) {
			t.Errorf("correction missing %q: %s", required, correction)
		}
	}
	if !strings.Contains(out.String(), "needs one correction") {
		t.Fatalf("output = %q", out.String())
	}

	if err := os.WriteFile(staging, []byte(validPreviewSetupYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	correction, resolved, err = inspectPreviewSetup(repo, launchconfig.ScopeHost, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if correction != "" || resolved == nil || resolved.Source.Scope != launchconfig.ScopeHost {
		t.Fatalf("correction = %q, resolved = %+v", correction, resolved)
	}
}

const validPreviewSetupYAML = `default: web-app
previews:
  web-app:
    directory: .
    command: npm run dev -- --host 127.0.0.1 --port "$PORT"
    ready:
      path: /
`
