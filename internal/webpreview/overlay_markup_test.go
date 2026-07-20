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
