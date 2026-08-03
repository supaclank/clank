package acp

import (
	"context"
	"fmt"

	"github.com/supaclank/clank/internal/agent"
	sdk "github.com/coder/acp-go-sdk"
)

// Fork branches the session where the adapter advertises the unstable
// fork capability. ACP fork has no message anchor, so only tip forks are
// honest — a mid-history messageID gets a typed unsupported error
// instead of silently forking the whole tip.
func (b *Backend) Fork(ctx context.Context, messageID string) (agent.ForkResult, error) {
	b.mu.Lock()
	conn := b.conn
	sid := b.sessionID
	last := b.red.lastMessageID()
	title := b.red.title
	b.mu.Unlock()

	if conn == nil || sid == "" {
		return agent.ForkResult{}, fmt.Errorf("acp %s: backend not open", b.profile.ID)
	}
	if conn.Init().AgentCapabilities.SessionCapabilities.Fork == nil {
		return agent.ForkResult{}, fmt.Errorf("fork is not supported by the %s backend: %w", b.profile.Backend, agent.ErrUnsupported)
	}
	if messageID != "" && messageID != last {
		return agent.ForkResult{}, fmt.Errorf("mid-history fork is not supported over ACP (only the latest message): %w", agent.ErrUnsupported)
	}

	resp, err := conn.Conn().UnstableForkSession(ctx, sdk.UnstableForkSessionRequest{
		SessionId: sdk.SessionId(sid),
		Cwd:       b.workDir,
	})
	if err != nil {
		return agent.ForkResult{}, fmt.Errorf("acp %s: session/fork: %w", b.profile.ID, err)
	}
	return agent.ForkResult{ID: string(resp.SessionId), Title: title}, nil
}

// Revert is an approved cut under ACP (no protocol support).
func (b *Backend) Revert(ctx context.Context, messageID string) error {
	return fmt.Errorf("revert is not supported by the %s backend: %w", b.profile.Backend, agent.ErrUnsupported)
}

// RespondQuestion is an approved cut under ACP (AskUserQuestion retired);
// the backend never emits Part.Question, so no prompt can arrive.
func (b *Backend) RespondQuestion(ctx context.Context, requestID string, answers []agent.QuestionAnswer, reject bool) error {
	return fmt.Errorf("questions are not supported by the %s backend: %w", b.profile.Backend, agent.ErrUnsupported)
}
