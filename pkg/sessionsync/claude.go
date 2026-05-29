package sessionsync

import (
	"context"
	"fmt"
	"io"

	"github.com/acksell/clank/internal/agent"
	claudecode "github.com/severity1/claude-agent-sdk-go"
)

// ClaudeBackend is the daemon-free Claude Code session source. Discovery
// reads the on-disk JSONL transcripts via the Agent SDK. Cross-machine
// session transfer (export/import) is not built yet — Claude is
// discovery-only — so those return ErrExportNotImplemented.
type ClaudeBackend struct{}

func (ClaudeBackend) Type() agent.BackendType { return agent.BackendClaudeCode }

// ListSessions reads Claude Code sessions visible from projectDir via the
// SDK (which expands to sibling git worktrees, mirroring `claude
// --resume`). An empty projectDir lists every session under the SDK's
// config dir.
func (ClaudeBackend) ListSessions(_ context.Context, projectDir string) ([]DiscoveredSession, error) {
	var (
		infos []claudecode.SDKSessionInfo
		err   error
	)
	if projectDir == "" {
		infos, err = claudecode.ListSessions()
	} else {
		infos, err = claudecode.ListSessions(claudecode.WithSessionDirectory(projectDir))
	}
	if err != nil {
		return nil, fmt.Errorf("list claude sessions: %w", err)
	}
	out := make([]DiscoveredSession, 0, len(infos))
	for _, info := range infos {
		out = append(out, claudeDiscovered(info))
	}
	return out, nil
}

// ExportSession is not implemented — Claude session transfer is future work.
func (ClaudeBackend) ExportSession(context.Context, string, string, io.Writer) error {
	return ErrExportNotImplemented
}

// ImportSession is not implemented — Claude session transfer is future work.
func (ClaudeBackend) ImportSession(context.Context, string, string) (string, error) {
	return "", ErrExportNotImplemented
}

func claudeDiscovered(info claudecode.SDKSessionInfo) DiscoveredSession {
	dir := ""
	if info.Cwd != nil {
		dir = *info.Cwd
	}
	updated := millisToTime(info.LastModified)
	created := updated
	if info.CreatedAt != nil {
		created = millisToTime(*info.CreatedAt)
	}
	return DiscoveredSession{
		Backend:    agent.BackendClaudeCode,
		ExternalID: info.SessionID,
		Title:      info.Summary,
		ProjectDir: dir,
		CreatedAt:  created,
		UpdatedAt:  updated,
	}
}
