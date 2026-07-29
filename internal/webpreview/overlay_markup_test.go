package webpreview

import (
	"strings"
	"testing"
)

// TestOverlayComposerSelectorIsClassScoped guards the composer wiring:
// ui.input was once selected with a bare $('textarea'), and adding the
// plan-notes textarea earlier in the markup silently captured it —
// Enter-to-send and the send button both read the wrong (hidden, empty)
// element. The composer must carry its own class and be selected by it.
func TestOverlayComposerSelectorIsClassScoped(t *testing.T) {
	t.Parallel()
	js := string(overlayJS)
	if strings.Contains(js, "$('textarea')") {
		t.Error("overlay.js selects a bare $('textarea'); scope it to the composer class — the first textarea in the markup is not the composer")
	}
	if !strings.Contains(js, `<textarea class="compose"`) {
		t.Error(`overlay.js composer textarea must carry class="compose"`)
	}
	if !strings.Contains(js, "$('.compose')") {
		t.Error("overlay.js must select the composer via $('.compose')")
	}
}

// TestOverlayInlineCommentWiring pins the inline-comment feature's
// contact points: the popover exists in the markup, and send() decides
// sendability via composerTextForSend — a raw ui.input.value guard
// would silently break comment-only submits (empty composer, comment
// chips attached).
func TestOverlayInlineCommentWiring(t *testing.T) {
	t.Parallel()
	js := string(overlayJS)
	// A textarea, not an input: the comment must wrap and grow as you
	// type past the popover's width.
	if !strings.Contains(js, `<div class="cpop">`) || !strings.Contains(js, `<textarea class="cpop-in"`) {
		t.Error("overlay.js must carry the comment popover markup (.cpop with a .cpop-in textarea)")
	}
	if !strings.Contains(js, "$('.cpop')") || !strings.Contains(js, "$('.cpop-in')") {
		t.Error("overlay.js must select the comment popover via $('.cpop') / $('.cpop-in')")
	}
	if !strings.Contains(js, "composerTextForSend(ui.input.value, store.chips)") {
		t.Error("send paths must gate on composerTextForSend(ui.input.value, store.chips) so comment-only submits work")
	}
	// Focusing the popover input deactivates the browser's own selection
	// highlight; without the pending mark the selection visibly vanishes
	// the moment the popover opens.
	if !strings.Contains(js, "'clank-pending'") || !strings.Contains(js, "'clank-comment'") {
		t.Error("overlay.js must register both the clank-comment and clank-pending highlights")
	}
	// Chips are the edit surface (clickable text can't work reliably —
	// highlight marks receive no events and ranges die on live reload).
	if !strings.Contains(js, "editChipComment(c, el.getBoundingClientRect())") {
		t.Error("overlay.js chips must open the prefilled comment editor on click")
	}
	// One chip per ⌘-selected element: a re-click edits, never duplicates.
	if !strings.Contains(js, "store.chips.find((c) => c.node === hoverEl)") {
		t.Error("overlay.js inspector clicks must dedupe by anchored element node")
	}
}
