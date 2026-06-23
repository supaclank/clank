package agent

import (
	"encoding/base64"

	claudecode "github.com/severity1/claude-agent-sdk-go"
)

// claudeDefaultSessionID mirrors the SDK's unexported default session id
// used by Client.Query. QueryStream messages must carry it so an
// image-bearing message lands in the same conversation as text queries.
const claudeDefaultSessionID = "default"

// dispatchClaudeQuery sends a user message to the Claude CLI. Text-only
// messages take the SDK's plain Query fast path; messages with image
// attachments are base64-inlined as image content blocks and sent via
// QueryStream (Query accepts only a string, so it can't carry images).
// Attachments are resolved by the caller before this runs.
func (b *ClaudeCodeBackend) dispatchClaudeQuery(client claudecode.Client, text string, imgs []resolvedImage) error {
	if len(imgs) == 0 {
		return client.Query(b.ctx, text)
	}
	ch := make(chan claudecode.StreamMessage, 1)
	ch <- buildUserStreamMessage(text, imgs)
	close(ch)
	return client.QueryStream(b.ctx, ch)
}

// buildUserStreamMessage assembles a stream-json user message whose
// content is the Anthropic block array: an optional text block followed by
// one base64 image block per attachment. Pure — unit-tested without a CLI.
func buildUserStreamMessage(text string, imgs []resolvedImage) claudecode.StreamMessage {
	content := make([]map[string]any, 0, len(imgs)+1)
	if text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, img := range imgs {
		content = append(content, map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": img.Mime,
				"data":       base64.StdEncoding.EncodeToString(img.Data),
			},
		})
	}
	return claudecode.StreamMessage{
		Type:      "user",
		SessionID: claudeDefaultSessionID,
		Message: map[string]any{
			"role":    "user",
			"content": content,
		},
	}
}
