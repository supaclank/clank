package agent

import (
	"time"

	claudecode "github.com/severity1/claude-agent-sdk-go"
)

// The CLI generates the title concurrently with the first turn, so on a fast
// turn it lands in the transcript after the result. scheduleAITitleRecheck
// re-reads on this cadence until it resolves or the attempts run out.
const (
	aiTitleRecheckDelay    = 3 * time.Second
	aiTitleRecheckAttempts = 5
)

// maybeEmitAITitle publishes EventTitleChange the first time the CLI-generated
// session title is available on disk, reporting whether the title has resolved
// (emitted now or on an earlier call).
//
// The Claude CLI writes its generated "ai-title" to the session transcript, not
// the stdout stream the SDK parses, so a natively-created session never learns
// the title from the live message stream — without this it falls back to showing
// the first prompt. Called from handleResult each turn (with a
// scheduleAITitleRecheck follow-up for fast turns) and from Open when resuming,
// so a title written while no turn was running still surfaces. Resolve-once —
// the CLI keeps the title stable for a session's life, so reading stops after
// the first successful emit.
//
// Callers run concurrently (handleResult, recheck goroutine, Open catch-up;
// Revert can spawn a new receiveLoop before the old one's in-flight call here
// has returned), so aiTitleEmitted, sessionID and projectDir are all
// read/written under b.mu.
func (b *ClaudeCodeBackend) maybeEmitAITitle() bool {
	b.mu.Lock()
	if b.aiTitleEmitted {
		b.mu.Unlock()
		return true
	}
	sessionID := b.sessionID
	workDir := b.projectDir
	b.mu.Unlock()

	title := readAITitle(sessionID, workDir)
	if title == "" {
		return false
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
	return sent
}

// scheduleAITitleRecheck starts a bounded background loop that re-reads the
// transcript until the ai-title resolves. Closes the fast-first-turn race: the
// CLI titles the session concurrently with the turn, so a trivial prompt's
// result often beats the title to the transcript — and a single-turn session
// would otherwise never surface it. At most one loop runs per backend; it stops
// on resolve, backend shutdown, or after aiTitleRecheckAttempts reads (the
// title either lands within seconds or the CLI skipped titling entirely, e.g.
// prompts under its minimum length).
func (b *ClaudeCodeBackend) scheduleAITitleRecheck() {
	b.mu.Lock()
	if b.aiTitleEmitted || b.aiTitleRecheckActive {
		b.mu.Unlock()
		return
	}
	b.aiTitleRecheckActive = true
	delay := b.AITitleRecheckDelay
	b.mu.Unlock()
	if delay <= 0 {
		delay = aiTitleRecheckDelay
	}

	go func() {
		defer func() {
			b.mu.Lock()
			b.aiTitleRecheckActive = false
			b.mu.Unlock()
		}()
		for range aiTitleRecheckAttempts {
			select {
			case <-b.ctx.Done():
				return
			case <-time.After(delay):
			}
			if b.maybeEmitAITitle() {
				return
			}
		}
	}()
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
