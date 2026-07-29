package clankcli

import (
	"context"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/agent/presets"
	"github.com/acksell/clank/internal/host/hosttest"
)

// TestStartPreviewAgent_AppliesDefaultPresetConfig pins the prompt-
// argument arm of `clank preview`: the created session carries the
// backend's Default preset config — a bare create is rejected by the
// host as config_incomplete (the host never fills values in).
func TestStartPreviewAgent_AppliesDefaultPresetConfig(t *testing.T) {
	client, stub := newTestHost(t)
	repo := hosttest.InitGitRepo(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sessionID, err := startPreviewAgent(ctx, client, agent.BackendOpenCode, repo, "make the header sticky")
	if err != nil {
		t.Fatalf("startPreviewAgent: %v", err)
	}
	if sessionID == "" {
		t.Fatal("no session id returned")
	}

	last := stub.Last()
	if last == nil {
		t.Fatal("no backend created — session was not started")
	}
	if got := last.LastSendOpts().Text; got != "make the header sticky" {
		t.Errorf("prompt received by backend: got %q", got)
	}
	wantCfg := presets.DefaultFor(presets.Workstation(), agent.BackendOpenCode).Config
	got := last.LastSendOpts().Config
	if len(got) == 0 {
		t.Fatal("no config reached the backend — the Default preset was not applied")
	}
	for k, v := range wantCfg {
		if got[k] != v {
			t.Errorf("config[%q]: got %q, want %q (Default preset value)", k, got[k], v)
		}
	}
}
