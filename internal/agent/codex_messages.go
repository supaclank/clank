package agent

import (
	"context"
	"encoding/json"
	"fmt"

	codex "github.com/pmenglund/codex-sdk-go"
)

// codexThreadView is the subset of a thread/read response the transcript
// conversion reads. The SDK types the response loosely (generated fallback),
// so we re-marshal into this local shape.
type codexThreadView struct {
	Thread struct {
		ID      string  `json:"id"`
		Name    *string `json:"name"`
		Preview string  `json:"preview"`
		Turns   []struct {
			ID     string            `json:"id"`
			Status string            `json:"status"`
			Items  []json.RawMessage `json:"items"`
		} `json:"turns"`
	} `json:"thread"`
}

// Messages serves the session transcript via thread/read (stable surface,
// full items per turn). Requires a live app-server: Open is idempotent and
// (re)spawns one when needed — the rollout files under CODEX_HOME have no
// stability guarantee, so clank never parses them directly.
//
// ID scheme: assistant messages are keyed by turn id — identical on the live
// stream (see turn/started) and here, so client refetches reconcile. User
// messages take the persisted item id, which codex renumbers relative to the
// live stream; clank clients replace history wholesale on refetch, so the
// drift is harmless.
func (b *CodexBackend) Messages(ctx context.Context) ([]MessageData, error) {
	b.mu.Lock()
	threadID := b.threadID
	b.mu.Unlock()
	if threadID == "" {
		return nil, nil
	}

	if err := b.Open(ctx); err != nil {
		return nil, fmt.Errorf("open codex backend for transcript read: %w", err)
	}
	b.mu.Lock()
	client := b.client
	b.mu.Unlock()
	if client == nil {
		return nil, fmt.Errorf("codex session is not open")
	}

	rd, err := client.ReadThread(ctx, threadID, codex.ThreadReadOptions{IncludeTurns: true})
	if err != nil {
		return nil, fmt.Errorf("read codex thread %s: %w", threadID, err)
	}
	raw, err := json.Marshal(rd)
	if err != nil {
		return nil, fmt.Errorf("re-marshal thread read response: %w", err)
	}
	var view codexThreadView
	if err := json.Unmarshal(raw, &view); err != nil {
		return nil, fmt.Errorf("decode thread read response: %w", err)
	}
	return codexTranscriptMessages(view), nil
}

// codexTranscriptMessages converts a thread view into clank messages: user
// items become user messages; everything else in a turn aggregates into one
// assistant message (id = turn id) whose parts mirror the live stream.
func codexTranscriptMessages(view codexThreadView) []MessageData {
	var out []MessageData
	for _, turn := range view.Thread.Turns {
		assistant := MessageData{
			ID:         turn.ID,
			Role:       "assistant",
			ProviderID: codexProviderOpenAI,
		}
		var sawAssistantContent bool

		for _, rawItem := range turn.Items {
			var item codexItem
			if err := json.Unmarshal(rawItem, &item); err != nil {
				continue
			}
			switch item.Type {
			case "userMessage":
				out = append(out, MessageData{ID: item.ID, Role: "user", Content: codexUserText(item)})
			case "agentMessage":
				assistant.Parts = append(assistant.Parts, Part{ID: item.ID, Type: PartText, Text: item.Text})
				// Content carries the reply text; the final answer wins when
				// both commentary and final phases exist.
				if assistant.Content == "" || item.Phase == "final_answer" {
					assistant.Content = item.Text
				}
				sawAssistantContent = true
			case "reasoning":
				if text := codexReasoningText(item); text != "" {
					assistant.Parts = append(assistant.Parts, Part{ID: item.ID, Type: PartThinking, Text: text})
					sawAssistantContent = true
				}
			default:
				if part, ok := codexToolPart(item, true); ok {
					assistant.Parts = append(assistant.Parts, part)
					sawAssistantContent = true
				}
			}
		}

		if sawAssistantContent {
			out = append(out, assistant)
		}
	}
	return out
}
