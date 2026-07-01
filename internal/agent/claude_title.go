package agent

import (
	"time"

	claudecode "github.com/severity1/claude-agent-sdk-go"
)

// maybeEmitAITitle publishes EventTitleChange the first time the CLI-generated
// session title is available on disk.
//
// The Claude CLI writes its generated "ai-title" to the session transcript, not
// the stdout stream the SDK parses, so a natively-created session never learns
// the title from the live message stream — without this it falls back to showing
// the first prompt. Called from handleResult each turn until the title resolves:
// a fast first turn can beat the CLI's asynchronous titling, so the title may
// only land on a later turn. Resolve-once — the CLI keeps the title stable for a
// session's life, so reading stops after the first successful emit.
//
// Called from handleResult; Revert can spawn a new receiveLoop before the old
// one's in-flight call here has returned, so aiTitleEmitted, sessionID and
// projectDir are all read/written under b.mu.
func (b *ClaudeCodeBackend) maybeEmitAITitle() {
	b.mu.Lock()
	if b.aiTitleEmitted {
		b.mu.Unlock()
		return
	}
	sessionID := b.sessionID
	workDir := b.projectDir
	b.mu.Unlock()

	title := readAITitle(sessionID, workDir)
	if title == "" {
		return
	}

	// Only mark emitted once the send actually lands — emit() drops events
	// when the buffer is full, and a marked-but-unsent title would never
	// resolve for the rest of the session's life.
	sent := b.emit(Event{
		Type:      EventTitleChange,
		Timestamp: time.Now(),
		Data:      TitleChangeData{Title: title},
	})
	if sent {
		b.mu.Lock()
		b.aiTitleEmitted = true
		b.mu.Unlock()
	}
}

// readAITitle returns the CLI-generated ai-title from a session's on-disk
// transcript, or "" if none has been written yet. Scopes the lookup to workDir
// like Messages and readSessionMessages. Best-effort: returns "" on any read
// error, matching the transcript-read convention elsewhere in this backend.
func readAITitle(sessionID, workDir string) string {
	if sessionID == "" {
		return ""
	}
	opts := []claudecode.SessionOption{}
	if workDir != "" {
		opts = append(opts, claudecode.WithSessionDirectory(workDir))
	}
	info, err := claudecode.GetSessionInfo(sessionID, opts...)
	if err != nil || info == nil || info.AITitle == nil {
		return ""
	}
	return *info.AITitle
}
