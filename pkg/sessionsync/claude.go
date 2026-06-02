package sessionsync

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/acksell/clank/internal/agent"
	claudecode "github.com/severity1/claude-agent-sdk-go"
)

// ClaudeBackend is the daemon-free Claude Code session source. Discovery
// reads the on-disk JSONL transcripts via the Agent SDK; export/import copy
// the raw transcript file (the SDK is read-only and exposes no writer), so
// they read/write <configDir>/projects/<encodeClaudeCwd(cwd)>/<id>.jsonl
// directly via the helpers in claude_paths.go.
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

// ExportSession copies the raw JSONL transcript for externalID to dst.
// projectDir is the session's working directory (Claude files transcripts
// by encoded cwd, so it is REQUIRED — unlike opencode, which exports
// global-by-id and ignores it). A missing transcript yields
// ErrSessionNotFound so the orchestrator skips the orphan.
func (ClaudeBackend) ExportSession(_ context.Context, projectDir, externalID string, dst io.Writer) error {
	if projectDir == "" {
		return fmt.Errorf("claude export: projectDir (session cwd) is required")
	}
	if externalID == "" {
		return fmt.Errorf("claude export: externalID is required")
	}
	path, err := claudeTranscriptPath(projectDir, externalID)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("claude export %s: %w", externalID, ErrSessionNotFound)
		}
		return fmt.Errorf("claude export: open transcript: %w", err)
	}
	defer f.Close()
	if _, err := io.Copy(dst, f); err != nil {
		return fmt.Errorf("claude export: copy transcript: %w", err)
	}
	return nil
}

// ImportSession writes the transcript blob to destDir's encoded project
// path and returns the Claude session id (read from the blob — the filename
// is identity, preserved across the round trip). destDir is REQUIRED; the
// orchestrator has already rebased the in-file cwd to destDir. The write
// overwrites any existing transcript: under clank's single-owner migration
// the incoming transcript is the authoritative superset.
func (ClaudeBackend) ImportSession(_ context.Context, destDir, blobPath string) (string, error) {
	if destDir == "" {
		return "", fmt.Errorf("claude import: destDir is required")
	}
	sessionID, err := claudeSessionIDFromBlob(blobPath)
	if err != nil {
		return "", fmt.Errorf("claude import: %w", err)
	}
	if filepath.Base(sessionID) != sessionID || strings.Contains(sessionID, "..") {
		return "", fmt.Errorf("claude import: invalid sessionID %q", sessionID)
	}
	dstPath, err := claudeTranscriptPath(destDir, sessionID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return "", fmt.Errorf("claude import: create project dir: %w", err)
	}
	if err := copyFileContents(blobPath, dstPath); err != nil {
		return "", fmt.Errorf("claude import: write transcript: %w", err)
	}
	return sessionID, nil
}

// claudeSessionIDFromBlob returns the sessionId from the first transcript
// line that carries one. Every Claude JSONL line has a sessionId, so this
// reads only the head of the file in practice.
func claudeSessionIDFromBlob(blobPath string) (string, error) {
	f, err := os.Open(blobPath)
	if err != nil {
		return "", fmt.Errorf("open blob: %w", err)
	}
	defer f.Close()
	br := bufio.NewReader(f)
	for {
		line, readErr := br.ReadString('\n')
		if id := sessionIDFromLine(line); id != "" {
			return id, nil
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", fmt.Errorf("read blob: %w", readErr)
		}
	}
	return "", fmt.Errorf("blob carries no %s in any line", claudeFieldSessionID)
}

func sessionIDFromLine(line string) string {
	if strings.TrimSpace(line) == "" {
		return ""
	}
	var probe struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal([]byte(line), &probe); err != nil {
		return ""
	}
	return probe.SessionID
}

// copyFileContents copies src to dst, truncating dst if it exists.
func copyFileContents(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return nil
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
