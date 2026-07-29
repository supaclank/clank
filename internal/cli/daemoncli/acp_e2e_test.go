package daemoncli

// Full-stack ACP e2e: a real ACPBackendManager (scripted ACP agent over
// real pipes through the real SDK) behind the real host service, mux,
// gateway, and daemonclient. Pins the two contracts the migration hangs
// on: the persisted ExternalID drives ensureBackend resume after a
// daemon restart, and dead-session history is rebuilt via session/load
// replay — with no clank-side transcript persistence anywhere.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	acpx "github.com/acksell/clank/internal/agent/acp"
	"github.com/acksell/clank/internal/agent/acp/acptest"
	"github.com/acksell/clank/internal/host"
	"github.com/acksell/clank/internal/host/hosttest"
	sdk "github.com/coder/acp-go-sdk"
)

// acpAgentStore is the scripted agent's own durable session store: it
// outlives daemon restarts (like codex's thread store outlives
// clank-host) so session/load can replay history to a fresh daemon.
type acpAgentStore struct {
	mu       sync.Mutex
	sessions int
	// turns per session id, in order: {prompt, reply} pairs.
	turns map[string][][2]string
}

func newACPAgentStore() *acpAgentStore {
	return &acpAgentStore{turns: make(map[string][][2]string)}
}

func (s *acpAgentStore) newSession() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions++
	id := fmt.Sprintf("acp-e2e-%d", s.sessions)
	s.turns[id] = nil
	return id
}

func (s *acpAgentStore) record(id, prompt, reply string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turns[id] = append(s.turns[id], [2]string{prompt, reply})
}

func (s *acpAgentStore) history(id string) [][2]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][2]string(nil), s.turns[id]...)
}

// scriptedFactory builds per-spawn agents that echo prompts and replay
// recorded history on session/load.
func (s *acpAgentStore) scriptedFactory() func(string) *acptest.ScriptedAgent {
	return func(string) *acptest.ScriptedAgent {
		a := &acptest.ScriptedAgent{}
		a.NewSessionFn = func(ctx context.Context, p sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
			return sdk.NewSessionResponse{SessionId: sdk.SessionId(s.newSession())}, nil
		}
		a.PromptFn = func(ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
			prompt := ""
			for _, b := range p.Prompt {
				if b.Text != nil {
					prompt += b.Text.Text
				}
			}
			reply := "echo:" + prompt
			s.record(string(p.SessionId), prompt, reply)
			_ = a.Conn().SessionUpdate(ctx, sdk.SessionNotification{
				SessionId: p.SessionId,
				Update:    sdk.UpdateAgentMessageText(reply),
			})
			return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
		}
		a.LoadSessionFn = func(ctx context.Context, p sdk.LoadSessionRequest) (sdk.LoadSessionResponse, error) {
			for _, turn := range s.history(string(p.SessionId)) {
				_ = a.Conn().SessionUpdate(ctx, sdk.SessionNotification{
					SessionId: p.SessionId,
					Update:    sdk.UpdateUserMessageText(turn[0]),
				})
				_ = a.Conn().SessionUpdate(ctx, sdk.SessionNotification{
					SessionId: p.SessionId,
					Update:    sdk.UpdateAgentMessageText(turn[1]),
				})
			}
			return sdk.LoadSessionResponse{}, nil
		}
		return a
	}
}

// newACPManager wires an ACPBackendManager to the scripted store.
func newACPManager(t *testing.T, store *acpAgentStore) *host.ACPBackendManager {
	t.Helper()
	profile := acpx.CodexProfile("unused-bun", "unused-entry", nil)
	mgr, err := host.NewACPBackendManager(profile)
	if err != nil {
		t.Fatalf("NewACPBackendManager: %v", err)
	}
	mgr.Supervisor().SetSpawnFunc(acptest.SpawnFunc(store.scriptedFactory(), profile, t.Logf))
	mgr.Supervisor().SetReconcileInterval(20 * time.Millisecond)
	return mgr
}

func createACPSession(t *testing.T, td *testDaemon, prompt string) *agent.SessionInfo {
	t.Helper()
	repo := hosttest.InitGitRepo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := td.Client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendCodex,
		GitRef:  agent.GitRef{LocalPath: repo, WorktreeID: "git@example.com:acme/repo.git"},
		Prompt:  prompt,
		Config:  workstationConfig(agent.BackendCodex),
	})
	if err != nil {
		t.Fatalf("Sessions().Create: %v", err)
	}
	return info
}

// waitForReply polls the messages endpoint until an assistant message
// containing want appears (or fails the test).
func waitForReply(t *testing.T, td *testDaemon, sessionID, want string) []agent.MessageData {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		msgs, err := td.Client.Session(sessionID).Messages(ctx)
		cancel()
		if err == nil {
			for _, m := range msgs {
				if m.Role == "assistant" && strings.Contains(m.Content, want) {
					return msgs
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("assistant reply %q never appeared", want)
	return nil
}

func TestACPE2E_TurnPersistsExternalID(t *testing.T) {
	t.Parallel()
	store := newACPAgentStore()
	dbPath := filepath.Join(t.TempDir(), "host.db")
	td := newTestDaemonWithManagers(t, dbPath, map[agent.BackendType]agent.BackendManager{
		agent.BackendCodex: newACPManager(t, store),
	})

	info := createACPSession(t, td, "hello")
	waitForReply(t, td, info.ID, "echo:hello")

	// The ACP session id must be persisted (that's what resume rides).
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	row, err := td.Store.GetSession(ctx, info.ID)
	if err != nil {
		t.Fatalf("Store.GetSession: %v", err)
	}
	if !strings.HasPrefix(row.ExternalID, "acp-e2e-") {
		t.Fatalf("persisted ExternalID = %q, want the ACP session id", row.ExternalID)
	}
	if row.Backend != agent.BackendCodex {
		t.Fatalf("persisted Backend = %q, want codex", row.Backend)
	}
}

// The migration's money test: restart the daemon (same host.db, fresh
// ACP manager), read history — ensureBackend must rebuild the backend
// from the persisted ExternalID and session/load must replay the
// transcript from the agent's own store. A follow-up turn then works.
func TestACPE2E_RestartResumesViaLoadReplay(t *testing.T) {
	t.Parallel()
	store := newACPAgentStore()
	dbPath := filepath.Join(t.TempDir(), "host.db")

	td1 := newTestDaemonWithManagers(t, dbPath, map[agent.BackendType]agent.BackendManager{
		agent.BackendCodex: newACPManager(t, store),
	})
	info := createACPSession(t, td1, "first question")
	waitForReply(t, td1, info.ID, "echo:first question")

	// "Restart": second daemon on the same host.db with a fresh manager
	// (fresh supervisor, fresh adapter processes — nothing in memory).
	td2 := newTestDaemonWithManagers(t, dbPath, map[agent.BackendType]agent.BackendManager{
		agent.BackendCodex: newACPManager(t, store),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	msgs, err := td2.Client.Session(info.ID).Messages(ctx)
	if err != nil {
		t.Fatalf("Messages after restart: %v", err)
	}
	var roles, contents []string
	for _, m := range msgs {
		roles = append(roles, m.Role)
		contents = append(contents, m.Content)
	}
	if len(msgs) != 2 || msgs[0].Content != "first question" || msgs[1].Content != "echo:first question" {
		t.Fatalf("replayed transcript = roles %v contents %v, want the original turn", roles, contents)
	}

	// Follow-up turn on the resumed session.
	if err := td2.Client.Session(info.ID).Send(ctx, agent.SendMessageOpts{Text: "second question"}); err != nil {
		t.Fatalf("Send after resume: %v", err)
	}
	msgs = waitForReply(t, td2, info.ID, "echo:second question")
	if len(msgs) != 4 {
		t.Fatalf("transcript after follow-up = %d messages, want 4 (replayed turn + new turn)", len(msgs))
	}
}
