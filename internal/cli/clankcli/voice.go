package clankcli

import (
	"context"
	"fmt"
	"os"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/voice"
	"github.com/eiannone/keyboard"
	"github.com/spf13/cobra"
)

func voiceCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "voice",
		Short: "Start a voice session to manage coding agents",
		Long: `Start an interactive voice session connected to the Clank daemon.

Use SPACE to toggle recording on/off. Speak your instructions and the
voice agent will manage your coding sessions: check status, relay
decisions, unblock agents, and more.

Requires OPENAI_API_KEY to be set in the daemon's environment.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVoice()
		},
	}
}

func runVoice() error {
	client, err := ensureDaemon()
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Subscribe to SSE events for transcript/status display.
	events, err := client.SubscribeEvents(ctx)
	if err != nil {
		return fmt.Errorf("subscribe events: %w", err)
	}

	// Start local audio devices.
	// Connect audio WebSocket to daemon. This also creates the voice
	// session on the daemon side (session is created when WS connects).
	wsConn, err := client.VoiceAudioStream(ctx)
	if err != nil {
		return fmt.Errorf("connect audio stream: %w", err)
	}

	bridge, err := voice.NewClientBridge(wsConn)
	if err != nil {
		wsConn.CloseNow()
		return fmt.Errorf("init audio bridge: %w", err)
	}
	defer bridge.Close()

	defer func() {
		client.VoiceStop(context.Background())
	}()

	// Goroutine: display SSE events (transcripts, status changes).
	go func() {
		var transcriptBuf string
		for evt := range events {
			switch evt.Type {
			case agent.EventVoiceTranscript:
				if data, ok := evt.Data.(agent.VoiceTranscriptData); ok {
					if data.Done {
						// Final: erase streaming preview, print full transcript.
						fmt.Printf("\033[2K\r[AGENT] %s\n", transcriptBuf)
						transcriptBuf = ""
					} else {
						// Accumulate delta; show truncated tail as single-line preview.
						transcriptBuf += data.Text
						preview := transcriptBuf
						const maxPreview = 72
						if len(preview) > maxPreview {
							preview = "..." + preview[len(preview)-maxPreview:]
						}
						fmt.Printf("\033[2K\r[AGENT] %s", preview)
					}
				}
			case agent.EventVoiceStatus:
				if data, ok := evt.Data.(agent.VoiceStatusData); ok {
					switch data.Status {
					case agent.VoiceStatusListening:
						fmt.Print("\033[2K\r[RECORDING] Speak now... (press SPACE to stop)")
					case agent.VoiceStatusThinking:
						fmt.Print("\033[2K\r[THINKING]")
					case agent.VoiceStatusSpeaking:
						// Agent is speaking — no special display needed.
					}
				}
			case agent.EventVoiceToolCall:
				if data, ok := evt.Data.(agent.VoiceToolCallData); ok {
					display := data.Args
					if len(display) > 200 {
						display = display[:200] + "..."
					}
					fmt.Printf("\n[TOOL] %s(%s)\n", data.Name, display)
				}
			case agent.EventError:
				if data, ok := evt.Data.(agent.ErrorData); ok {
					fmt.Fprintf(os.Stderr, "\n[ERROR] %s\n", data.Message)
				}
			}
		}
	}()

	// Set up keyboard for toggle-to-talk.
	if err := keyboard.Open(); err != nil {
		return fmt.Errorf("open keyboard: %w", err)
	}
	defer keyboard.Close()

	fmt.Println("=== Clank Voice ===")
	fmt.Println("Press SPACE to toggle recording on/off.")
	fmt.Println("Press Q or Ctrl+C to quit.")
	fmt.Println()

	recording := false
	for {
		char, key, err := keyboard.GetKey()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			continue
		}

		if key == keyboard.KeyCtrlC || char == 'q' || char == 'Q' {
			fmt.Println("\nGoodbye!")
			return nil
		}

		if key == keyboard.KeySpace || char == ' ' {
			if !recording {
				recording = true
				if err := bridge.Start(); err != nil {
					fmt.Fprintf(os.Stderr, "[ERROR] start: %v\n", err)
					continue
				}
			} else {
				recording = false
				if err := bridge.Stop(); err != nil {
					fmt.Fprintf(os.Stderr, "[ERROR] stop: %v\n", err)
					continue
				}
			}
		}
	}
}
