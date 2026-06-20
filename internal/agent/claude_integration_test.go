//go:build integration

// Real-CLI integration tests for the Claude backend. These drive the actual
// `claude` subprocess and the Anthropic API, so they are gated behind the
// `integration` build tag and skip unless an OAuth token is available:
//
//	go test -tags integration ./internal/agent/ -run Integration -v
//
// The token is read from $CLAUDE_CODE_OAUTH_TOKEN or, failing that, clank's
// own store (~/.local/share/clank/anthropic.json).
package agent_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
)

func integrationToken(t *testing.T) string {
	t.Helper()
	if tok := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); tok != "" {
		return tok
	}
	home, _ := os.UserHomeDir()
	data, err := os.ReadFile(filepath.Join(home, ".local/share/clank/anthropic.json"))
	if err == nil {
		var creds struct {
			OAuthToken string `json:"oauth_token"`
		}
		if json.Unmarshal(data, &creds) == nil && creds.OAuthToken != "" {
			return creds.OAuthToken
		}
	}
	t.Skip("no CLAUDE_CODE_OAUTH_TOKEN (or clank anthropic.json) — skipping real-CLI integration test")
	return ""
}

func msgText(m agent.MessageData) string {
	txt := m.Content
	for _, p := range m.Parts {
		if p.Type == agent.PartText {
			txt += p.Text
		}
	}
	return txt
}

// awaitReply polls the on-disk transcript until the user message matching
// promptMark has an assistant reply on the active chain, then returns that
// reply. Polling the transcript (rather than status events) avoids racing on a
// spurious Idle emitted by Open/reopen, which would otherwise let the test
// proceed and Stop the backend mid-turn — leaving an incomplete branch.
func awaitReply(t *testing.T, b *agent.ClaudeCodeBackend, promptMark string) string {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(150 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(time.Second)
		if b.Status() == agent.StatusError {
			t.Fatalf("backend errored while awaiting reply to %q", promptMark)
		}
		msgs, err := b.Messages(ctx)
		if err != nil {
			continue
		}
		ui := -1
		for i, m := range msgs {
			if m.Role == "user" && strings.Contains(msgText(m), promptMark) {
				ui = i
			}
		}
		if ui >= 0 {
			for j := ui + 1; j < len(msgs); j++ {
				if msgs[j].Role == "assistant" {
					if txt := msgText(msgs[j]); strings.TrimSpace(txt) != "" {
						return txt
					}
				}
			}
		}
	}
	t.Fatalf("timed out awaiting reply to %q", promptMark)
	return ""
}

func userMessageContaining(msgs []agent.MessageData, substr string) string {
	for _, m := range msgs {
		if m.Role == "user" && strings.Contains(msgText(m), substr) {
			return m.ID
		}
	}
	return ""
}

// TestIntegration_RevertSurvivesRestart is the regression test for the whole
// saga: revert truncates the conversation at a non-leaf cursor, and after a
// simulated daemon restart (a fresh backend resuming the same session with a
// plain --resume) the agent continues the *reverted* branch — not the orphaned
// one — and Open does not brick on an unresolvable --resume-session-at.
func TestIntegration_RevertSurvivesRestart(t *testing.T) {
	tok := integrationToken(t)
	ctx := context.Background()
	dir := "/private/tmp/rsa-itest"
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	b1 := agent.NewClaudeCodeBackend(dir)
	b1.ExtraEnv = map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": tok}

	if err := b1.OpenAndSend(ctx, agent.SendMessageOpts{Text: "The codeword is ALPHA. Reply with only: ok"}); err != nil {
		t.Fatalf("turn1: %v", err)
	}
	awaitReply(t, b1, "codeword is ALPHA")
	sid := b1.SessionID()
	if sid == "" {
		t.Fatal("no session id after first turn")
	}

	if err := b1.Send(ctx, agent.SendMessageOpts{Text: "Correction: the codeword is now BETA. Reply with only: ok"}); err != nil {
		t.Fatalf("turn2: %v", err)
	}
	awaitReply(t, b1, "codeword is now BETA")

	// Revert the BETA turn — must branch at the ALPHA-turn assistant (a non-leaf).
	msgs, _ := b1.Messages(ctx)
	betaID := userMessageContaining(msgs, "BETA")
	if betaID == "" {
		t.Fatal("could not find the BETA user message to revert to")
	}
	if err := b1.Revert(ctx, betaID); err != nil {
		t.Fatalf("revert: %v", err)
	}

	if err := b1.Send(ctx, agent.SendMessageOpts{Text: "Correction: the codeword is now GAMMA. Reply with only: ok"}); err != nil {
		t.Fatalf("turn3: %v", err)
	}
	awaitReply(t, b1, "codeword is now GAMMA")
	resumeID := b1.SessionID()
	t.Logf("session id: original=%s after-revert=%s (changed=%v)", sid, resumeID, resumeID != sid)
	b1.Stop()

	// Simulate a daemon restart: a brand-new backend resumes the same session.
	b2 := agent.NewClaudeCodeBackendForSession(dir, resumeID, "")
	b2.ExtraEnv = map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": tok}
	if err := b2.Open(ctx); err != nil {
		t.Fatalf("reopen after restart bricked: %v", err)
	}
	defer b2.Stop()

	if err := b2.Send(ctx, agent.SendMessageOpts{Text: "List every codeword I have given you, in order, comma-separated. Output only the list."}); err != nil {
		t.Fatalf("recall send: %v", err)
	}
	recall := awaitReply(t, b2, "List every codeword")
	t.Logf("post-restart recall: %q", recall)
	if !strings.Contains(recall, "GAMMA") {
		t.Errorf("expected the reverted branch (…GAMMA) after restart; got %q", recall)
	}
	if strings.Contains(recall, "BETA") {
		t.Errorf("orphaned branch leaked after restart (mentions BETA); got %q", recall)
	}
}
