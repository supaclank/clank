package voice

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/mindmouth"
	"github.com/acksell/mindmouth/tools"
)

// Instructions is the system prompt for the voice supervisor agent.
const Instructions = `You are the user's chief of staff for their fleet of coding agents managed by the Clank daemon.

Your role:
- Help the user stay on top of what their agents are doing
- Summarize session states and highlight what needs attention
- Review agent plans and facilitate quick decisions
- Relay user decisions, feedback, and approvals to the correct session agents
- Help unblock agents that are waiting for human input

When the user starts talking, first check if they're asking about a specific session or want an overview. Use list_sessions to see the current state. Use get_messages to read a session's conversation when the user wants details.

When the user makes a decision (approves a plan, answers an agent's question, gives feedback), use send_message to relay it. Summarize what you're sending before you send it.

Be conversational and concise. Speak in short sentences. Focus on what needs the user's attention: questions, blockers, completed work needing review. Don't list every session unless asked — highlight what's changed or needs action.`

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
}

// NewSession creates a voice session, connects to the OpenAI Realtime
// API, and starts the audio pipeline. The session is ready for
// StartListening/StopListening calls after this returns.
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

	// Create mindmouth agent.
	mmAgent, err := mindmouth.NewAgent(mindmouth.AgentConfig{
		APIKey:       cfg.APIKey,
		Instructions: Instructions,
		Source:       cfg.Source,
		Sink:         cfg.Sink,
		Tools:        reg.RealtimeTools(),
		ToolExecutor: reg.Execute,
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
			OnListening: func(listening bool) {
				if listening {
					s.setStatus(agent.VoiceStatusListening)
				} else {
					s.setStatus(agent.VoiceStatusThinking)
				}
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

// StartListening begins a user voice turn (unmute mic, flush playback).
func (s *Session) StartListening() {
	s.agent.StartListening()
}

// StopListening ends a user voice turn (mute mic, trigger model response).
func (s *Session) StopListening() {
	committed := s.agent.StopListening()
	if committed {
		s.setStatus(agent.VoiceStatusSpeaking)
	} else {
		// No audio was sent (or commit failed). No response will
		// arrive from OpenAI, so go straight back to idle.
		s.setStatus(agent.VoiceStatusIdle)
	}
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
