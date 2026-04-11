package voice

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/mindmouth"
	"github.com/acksell/mindmouth/tools"
)

// baseInstructions is the core system prompt for the voice supervisor agent.
// buildInstructions appends dynamic context (known projects, etc.).
const baseInstructions = `You are the user's chief of staff for their fleet of coding agents managed by the Clank daemon.

Your role:
- Help the user stay on top of what their agents are doing
- Summarize session states and highlight what needs attention
- Review agent plans and facilitate quick decisions
- Relay user decisions, feedback, and approvals to the correct session agents
- Help unblock agents that are waiting for human input

When the user starts talking, first check if they're asking about a specific session or want an overview. Use list_sessions to see the current state. Use get_messages to read a session's conversation when the user wants details.

When the user makes a decision (approves a plan, answers an agent's question, gives feedback), use send_message to relay it. Summarize what you're sending before you send it.

When creating a session, you MUST use an exact project_dir from the known projects list below. NEVER guess or construct a path yourself.

Be conversational and concise. Speak in short sentences. Focus on what needs the user's attention: questions, blockers, completed work needing review. Don't list every session unless asked — highlight what's changed or needs action.`

// buildInstructions appends the known project directories to the base
// system prompt so the voice agent can reference exact paths.
func buildInstructions(knownDirs []string) string {
	if len(knownDirs) == 0 {
		return baseInstructions + "\n\nNo known projects yet. The user must provide an exact absolute project path when creating a session."
	}
	var b strings.Builder
	b.WriteString(baseInstructions)
	b.WriteString("\n\nKnown projects:\n")
	for _, dir := range knownDirs {
		fmt.Fprintf(&b, "- %s (%s)\n", dir, filepath.Base(dir))
	}
	return b.String()
}

// Session manages a single voice agent backed by the OpenAI Realtime
// API. It wraps mindmouth.Agent and bridges audio from an external
// source (typically the CLI/TUI over WebSocket) to the Realtime API.
//
// The Session runs on the daemon. Audio I/O is provided by the caller
// via the AudioSource and AudioSink interfaces from mindmouth.
type Session struct {
	agent *mindmouth.Agent

	mu     sync.Mutex
	status agent.VoiceStatus

	// broadcast sends events to the daemon's SSE subscribers.
	broadcast func(agent.Event)

	log *log.Logger
}

// Config holds everything needed to create a voice Session.
type Config struct {
	// APIKey is the OpenAI API key (required).
	APIKey string

	// Source provides audio input (required). On the daemon side, this
	// is a WebSocket adapter that receives PCM from the CLI/TUI.
	Source mindmouth.AudioSource

	// Sink receives audio output (required). On the daemon side, this
	// is a WebSocket adapter that streams PCM back to the CLI/TUI.
	Sink mindmouth.AudioSink

	// ToolProvider gives the voice agent access to daemon operations.
	ToolProvider ToolProvider

	// Broadcast is called to emit events to daemon SSE subscribers.
	Broadcast func(agent.Event)

	// Logger for voice session messages. If nil, a default is used.
	Logger *log.Logger

	// Debug enables verbose tool-call logging (full request/response
	// bodies). When false, only a summary of each tool-call result is
	// logged. Controlled by the CLANK_DEBUG=1 environment variable.
	Debug bool
}

// NewSession creates a voice session and starts the event-driven audio
// pipeline. The OpenAI connection is deferred until the first turn
// start event arrives from the AudioSource.
func NewSession(ctx context.Context, cfg Config) (*Session, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("voice: APIKey is required")
	}
	if cfg.Source == nil {
		return nil, fmt.Errorf("voice: Source is required")
	}
	if cfg.Sink == nil {
		return nil, fmt.Errorf("voice: Sink is required")
	}
	if cfg.ToolProvider == nil {
		return nil, fmt.Errorf("voice: ToolProvider is required")
	}
	if cfg.Broadcast == nil {
		return nil, fmt.Errorf("voice: Broadcast is required")
	}

	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}

	s := &Session{
		status:    agent.VoiceStatusIdle,
		broadcast: cfg.Broadcast,
		log:       logger,
	}

	// Build tool registry.
	reg := tools.NewRegistry()
	RegisterTools(reg, cfg.ToolProvider)

	// Query known project directories to ground the agent's context.
	knownDirs, err := cfg.ToolProvider.KnownProjectDirs(ctx)
	if err != nil {
		logger.Printf("warning: failed to load known project dirs: %v", err)
	}
	instructions := buildInstructions(knownDirs)

	// Create mindmouth agent.
	toolExecutor := loggingToolExecutor(reg.Execute, logger, cfg.Debug)
	mmAgent, err := mindmouth.NewAgent(mindmouth.AgentConfig{
		APIKey:       cfg.APIKey,
		Instructions: instructions,
		Source:       cfg.Source,
		Sink:         cfg.Sink,
		Tools:        reg.RealtimeTools(),
		ToolExecutor: toolExecutor,
		Voice:        "coral",
		Callbacks: mindmouth.Callbacks{
			OnTranscript: func(text string, done bool) {
				s.broadcast(agent.Event{
					Type:      agent.EventVoiceTranscript,
					Timestamp: time.Now(),
					Data: agent.VoiceTranscriptData{
						Text: text,
						Done: done,
					},
				})
				if done {
					s.setStatus(agent.VoiceStatusIdle)
				}
			},
			OnText: func(text string, done bool) {
				// Text-only model responses (e.g. after tool calls).
				// Surface them as transcripts so the client displays them.
				s.broadcast(agent.Event{
					Type:      agent.EventVoiceTranscript,
					Timestamp: time.Now(),
					Data: agent.VoiceTranscriptData{
						Text: text,
						Done: done,
					},
				})
				if done {
					s.setStatus(agent.VoiceStatusIdle)
				}
			},
			OnToolCall: func(name string, args string) {
				s.broadcast(agent.Event{
					Type:      agent.EventVoiceToolCall,
					Timestamp: time.Now(),
					Data: agent.VoiceToolCallData{
						Name: name,
						Args: args,
					},
				})
			},
			OnError: func(err error) {
				s.log.Printf("voice agent error: %v", err)
				s.broadcast(agent.Event{
					Type:      agent.EventError,
					Timestamp: time.Now(),
					Data:      agent.ErrorData{Message: fmt.Sprintf("voice: %v", err)},
				})
			},
			OnResponseDone: func() {
				s.setStatus(agent.VoiceStatusIdle)
			},
			OnListeningStart: func() {
				s.setStatus(agent.VoiceStatusListening)
			},
			OnListeningEnd: func() {
				s.setStatus(agent.VoiceStatusThinking)
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("voice: create agent: %w", err)
	}

	// Connect to OpenAI and start audio pipeline.
	if err := mmAgent.Start(ctx); err != nil {
		return nil, fmt.Errorf("voice: start agent: %w", err)
	}

	s.agent = mmAgent
	s.log.Println("voice session started")
	return s, nil
}

// Status returns the current voice state.
func (s *Session) Status() agent.VoiceStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// Wait blocks until the voice session ends.
func (s *Session) Wait() error {
	return s.agent.Wait()
}

// Close tears down the voice session.
func (s *Session) Close() error {
	s.log.Println("voice session closing")
	s.setStatus(agent.VoiceStatusIdle)
	if s.agent != nil {
		return s.agent.Close()
	}
	return nil
}

func (s *Session) setStatus(status agent.VoiceStatus) {
	s.mu.Lock()
	old := s.status
	s.status = status
	s.mu.Unlock()

	s.log.Printf("voice: setStatus %s -> %s", old, status)

	if old == status {
		return
	}

	s.broadcast(agent.Event{
		Type:      agent.EventVoiceStatus,
		Timestamp: time.Now(),
		Data:      agent.VoiceStatusData{Status: status},
	})
}

// loggingToolExecutor wraps a ToolExecutor to log every tool call's
// name, args, result summary, and duration. When debug is true (or
// CLANK_DEBUG=1 is set), the full result body is logged.
func loggingToolExecutor(
	inner func(string, json.RawMessage) (string, error),
	logger *log.Logger,
	debug bool,
) func(string, json.RawMessage) (string, error) {
	if os.Getenv("CLANK_DEBUG") == "1" {
		debug = true
	}
	return func(name string, args json.RawMessage) (string, error) {
		logger.Printf("[voice-tool] %s args=%s", name, string(args))
		start := time.Now()

		result, err := inner(name, args)
		dur := time.Since(start)

		if err != nil {
			logger.Printf("[voice-tool] %s error=%v (%s)", name, err, dur)
			return result, err
		}

		if debug || len(result) < 500 {
			// Short result or debug mode: log the whole thing.
			logger.Printf("[voice-tool] %s result=%s (%s)", name, result, dur)
		} else {
			// Summarize: byte count + first 200 chars.
			preview := result[:200]
			// Count newlines as a rough proxy for "number of items" in list results.
			lines := strings.Count(result, "\n")
			logger.Printf("[voice-tool] %s returned %d bytes, %d lines, preview=%q (%s)",
				name, len(result), lines, preview, dur)
		}
		return result, nil
	}
}
