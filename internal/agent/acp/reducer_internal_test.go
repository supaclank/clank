package acp

import (
	"strings"
	"testing"

	sdk "github.com/coder/acp-go-sdk"
)

func userChunk(cb sdk.ContentBlock) sdk.SessionNotification {
	return sdk.SessionNotification{Update: sdk.SessionUpdate{
		UserMessageChunk: &sdk.SessionUpdateUserMessageChunk{Content: cb},
	}}
}

func agentChunk(cb sdk.ContentBlock) sdk.SessionNotification {
	return sdk.SessionNotification{Update: sdk.SessionUpdate{
		AgentMessageChunk: &sdk.SessionUpdateAgentMessageChunk{Content: cb},
	}}
}

// A replayed prompt arrives as one text chunk plus one chunk per attachment.
// The live path records only the typed text as Content (attachments go to the
// model as blocks), so replay must reconstruct the same — an image chunk used
// to marshal its full base64 payload (~200k chars per phone screenshot) into
// the committed transcript, which mobile clients then laid out as a single
// unbreakable word (multi-second main-thread stall per render;
// supaclank/clank-mobile#156).
func TestReplayedImageAttachmentStaysOutOfUserContent(t *testing.T) {
	t.Parallel()
	r := newReducer(nil)
	r.setSessionID("s1")
	r.replaying = true

	r.reduce(userChunk(sdk.TextBlock("Make the button pink.")))
	r.reduce(userChunk(sdk.ImageBlock(strings.Repeat("A", 200_000), "image/png")))
	r.reduce(agentChunk(sdk.TextBlock("On it."))) // commits the pending replay user

	if len(r.messages) == 0 {
		t.Fatal("no messages committed")
	}
	m := r.messages[0]
	if m.Role != "user" {
		t.Fatalf("messages[0].Role = %q, want user", m.Role)
	}
	if m.Content != "Make the button pink." {
		t.Fatalf("user Content = %q (len %d), want the typed text only", truncate(m.Content, 80), len(m.Content))
	}
}

// An attachment-only prompt has no text chunk at all; the image chunk must
// still open the user message so the turn boundary survives (matching the
// live path, which records an empty-Content user message for it).
func TestReplayedImageOnlyPromptKeepsMessageBoundary(t *testing.T) {
	t.Parallel()
	r := newReducer(nil)
	r.setSessionID("s1")
	r.replaying = true

	r.reduce(userChunk(sdk.ImageBlock("aGVsbG8=", "image/png")))
	r.reduce(agentChunk(sdk.TextBlock("Looking.")))

	if len(r.messages) == 0 {
		t.Fatal("no messages committed")
	}
	m := r.messages[0]
	if m.Role != "user" || m.Content != "" {
		t.Fatalf("messages[0] = {Role: %q, Content: %q}, want an empty user message", m.Role, truncate(m.Content, 80))
	}
}

// Blob blocks render as short tags everywhere contentBlockText is used
// (assistant chunks, tool-result content) — never as marshaled base64.
func TestContentBlockTextTagsBlobVariants(t *testing.T) {
	t.Parallel()
	if got := contentBlockText(sdk.ImageBlock("aGVsbG8=", "image/png")); got != "[image]" {
		t.Errorf("image block = %q, want [image]", got)
	}
	if got := contentBlockText(sdk.AudioBlock("aGVsbG8=", "audio/wav")); got != "[audio]" {
		t.Errorf("audio block = %q, want [audio]", got)
	}
	if got := contentBlockText(sdk.TextBlock("plain")); got != "plain" {
		t.Errorf("text block = %q, want plain", got)
	}
}

// A tool result carrying an image (e.g. a screenshot tool) joins as its tag,
// alongside the existing "[terminal output]" idiom.
func TestFlattenToolOutputTagsImageContent(t *testing.T) {
	t.Parallel()
	up := &sdk.SessionToolCallUpdate{Content: []sdk.ToolCallContent{
		{Content: &sdk.ToolCallContentContent{Content: sdk.TextBlock("saved to /tmp/shot.png")}},
		{Content: &sdk.ToolCallContentContent{Content: sdk.ImageBlock("aGVsbG8=", "image/png")}},
	}}
	if got := flattenToolOutput(up); got != "saved to /tmp/shot.png\n[image]" {
		t.Fatalf("flattenToolOutput = %q", got)
	}
}
