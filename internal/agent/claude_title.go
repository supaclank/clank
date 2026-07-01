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
// Runs only on the receiveLoop goroutine (via handleResult), so aiTitleEmitted
// needs no lock; sessionID and projectDir are read under b.mu.
func (b *ClaudeCodeBackend) maybeEmitAITitle() {
	if b.aiTitleEmitted {
		return
	}

	b.mu.Lock()
	sessionID := b.sessionID
	workDir := b.projectDir
	b.mu.Unlock()

	title := readAITitle(sessionID, workDir)
	if title == "" {
		return
	}

	b.aiTitleEmitted = true
	b.emit(Event{
		Type:      EventTitleChange,
		Timestamp: time.Now(),
		Data:      TitleChangeData{Title: title},
	})
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
