package agent_test

import (
	"context"
	"testing"

	"github.com/acksell/clank/internal/agent"
)

// TestOpenAndSend_BadAttachmentLeavesStatusIdle guards against a regression
// where a user-supplied bad attachment (wrong mime, oversized, bad URL) would
// flip a newly-opened session to StatusError, making it unrecoverable. The
// session must stay at StatusIdle so the user can retry.
func TestOpenAndSend_BadAttachmentLeavesStatusIdle(t *testing.T) {
	t.Parallel()
	tr := newMockTransport(nil)
	b := newTestBackend(t, tr)
	defer b.Stop()

	err := b.OpenAndSend(context.Background(), agent.SendMessageOpts{
		Text: "hello",
		Attachments: []agent.Attachment{{
			Mime:   "image/svg+xml", // not in AllowedMimes → resolveImage returns error immediately
			Source: "data:image/svg+xml;base64,PHN2Zy8+",
		}},
	})
	if err == nil {
		t.Fatal("expected error from bad attachment mime, got nil")
	}
	if got := b.Status(); got != agent.StatusIdle {
		t.Fatalf("status after bad attachment: got %s, want %s", got, agent.StatusIdle)
	}
}
