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

	"github.com/supaclank/clank/internal/agent"
	"github.com/supaclank/clank/internal/host/hosttest"
	"github.com/supaclank/clank/internal/launchconfig"
)

func TestCreatePreviewSetupSessionUsesProjectConfigPrompt(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir())
	client, stub := newTestHost(t)
	repo := hosttest.InitGitRepo(t)
	paths, err := launchconfig.ResolvePaths(repo)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, events, cancelEvents, err := createPreviewSetupSession(ctx, client, agent.BackendOpenCode, paths)
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
	if !strings.Contains(prompt, paths.Project) || !strings.Contains(prompt, "non-interactive") {
		t.Fatalf("setup prompt = %q", prompt)
	}
	if strings.Contains(strings.ToLower(prompt), "private host") {
		t.Fatalf("setup prompt mentions private storage: %q", prompt)
	}
}

func TestRunPreviewSetupCompletesInlineWithProjectConfig(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir())
	client, stub := newTestHost(t)
	repo := hosttest.InitGitRepo(t)
	paths, err := launchconfig.ResolvePaths(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.Project), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Project, []byte(validPreviewSetupYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out bytes.Buffer
	resolved, err := runPreviewSetup(ctx, client, agent.BackendOpenCode, repo, strings.NewReader("unused"), &out)
	if err != nil {
		t.Fatalf("runPreviewSetup: %v\noutput:\n%s", err, out.String())
	}
	if resolved.Launch.Source.Path != paths.Project {
		t.Fatalf("resolved source = %+v", resolved.Launch.Source)
	}
	for _, required := range []string{
		"\nOne-time setup: generating .clank/launch.yaml with your connected agent…\n\n",
		"Preview configuration generated",
	} {
		if !strings.Contains(out.String(), required) {
			t.Errorf("output missing %q: %q", required, out.String())
		}
	}
	if strings.Contains(strings.ToLower(out.String()), "private storage") || strings.Contains(strings.ToLower(out.String()), "this machine") {
		t.Fatalf("output mentions private storage: %q", out.String())
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
	t.Parallel()
	repo := hosttest.InitGitRepo(t)
	paths, err := launchconfig.ResolvePaths(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.Project), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Project, []byte("prevews: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	correction, resolved, err := inspectPreviewSetup(repo, &out)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != nil || correction == "" {
		t.Fatalf("correction = %q, resolved = %+v", correction, resolved)
	}
	for _, required := range []string{"validation failed", "field prevews not found", paths.Project, "non-interactive", "one minute"} {
		if !strings.Contains(correction, required) {
			t.Errorf("correction missing %q: %s", required, correction)
		}
	}
	if !strings.Contains(out.String(), "needs one correction") {
		t.Fatalf("output = %q", out.String())
	}

	if err := os.WriteFile(paths.Project, []byte(validPreviewSetupYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	correction, resolved, err = inspectPreviewSetup(repo, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if correction != "" || resolved == nil || resolved.Source.Path != paths.Project {
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
